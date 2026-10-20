package staleplan

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/asteby/metacore-kernel/cachecoherence"
	"gorm.io/gorm"
)

// generation counts statement-handle resets process-wide. The Dialector's
// per-connection hook compares against it.
var generation atomic.Uint64

var (
	regMu sync.Mutex
	pools = map[*pool]struct{}{}
	once  sync.Once
)

// Install wraps db's connection pool with the stale-plan retry (see the
// package doc) and registers it with cachecoherence so every Bump drops its
// prepared-statement handles first. It is idempotent, a no-op for non-Postgres
// dialects, and must be called on the ROOT *gorm.DB right after gorm.Open:
// sessions derived before the call keep the unwrapped pool.
func Install(db *gorm.DB) error {
	if db == nil {
		return errors.New("staleplan: nil db")
	}
	if db.Dialector == nil || db.Dialector.Name() != "postgres" {
		return nil
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, ok := db.ConnPool.(*pool); ok {
		return nil
	}
	old := db.ConnPool
	p := &pool{inner: old}
	db.ConnPool = p
	if db.Statement != nil && (db.Statement.ConnPool == nil || db.Statement.ConnPool == old) {
		db.Statement.ConnPool = p
	}
	pools[p] = struct{}{}
	once.Do(func() {
		cachecoherence.Register("db.statements", cachecoherence.OrderDatabase,
			func(ctx context.Context, ev cachecoherence.Event) error {
				Reset()
				return nil
			})
		cachecoherence.Default.RegisterProvider("staleplan", func() any { return GetStats() })
	})
	return nil
}

// Installed reports whether db's pool carries the wrapper.
func Installed(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	_, ok := db.ConnPool.(*pool)
	return ok
}

// Reset drops the prepared-statement handles of every installed pool and
// advances the generation so pooled connections deallocate their server-side
// statements on next reuse (see Dialector). Returns how many handles it closed.
func Reset() int {
	regMu.Lock()
	list := make([]*pool, 0, len(pools))
	for p := range pools {
		list = append(list, p)
	}
	regMu.Unlock()
	generation.Add(1)
	n := 0
	for _, p := range list {
		n += resetStatements(p.inner)
		if sdb, err := p.GetDBConn(); err == nil {
			scrubIdle(context.Background(), sdb)
		}
	}
	return n
}

// ResetStatements drops the prepared-statement handles of db only.
func ResetStatements(db *gorm.DB) int {
	if db == nil {
		return 0
	}
	generation.Add(1)
	if p, ok := db.ConnPool.(*pool); ok {
		n := resetStatements(p.inner)
		if sdb, err := p.GetDBConn(); err == nil {
			scrubIdle(context.Background(), sdb)
		}
		return n
	}
	return resetStatements(db.ConnPool)
}

// resetStatements closes every statement handle GORM's PrepareStmt cache holds.
// Closing an *sql.Stmt makes database/sql close the driver statement on every
// connection it was prepared on (DEALLOCATE), idle now, busy when released.
func resetStatements(cp gorm.ConnPool) int {
	ps, ok := cp.(*gorm.PreparedStmtDB)
	if !ok || ps == nil {
		return 0
	}
	// Evict under the lock, but do NOT close under it: sql.Stmt.Close waits
	// for the rows other goroutines still iterate, and those goroutines may
	// be waiting for this very lock to prepare their next statement. The
	// eviction callback closes asynchronously (gorm's own behaviour); the
	// connections are scrubbed by scrubIdle / the Dialector hook, not by the
	// close, so nothing here depends on its timing.
	ps.Mux.Lock()
	keys := ps.Stmts.Keys()
	n := len(keys)
	for _, k := range keys {
		ps.Stmts.Delete(k)
	}
	ps.Mux.Unlock()
	counters.resets.Add(1)
	promResets.Inc()
	if n > 0 {
		slog.Info("staleplan.reset", "statements", n)
	}
	return n
}

// RetryTx runs fn in db.Transaction and, if it fails with a stale-plan error
// raised by a statement that was NOT the first of the transaction, rolls back
// (gorm already did), drops the stale handles and runs fn ONCE more in a fresh
// transaction.
//
// Contract for fn: it must be repeatable, i.e. its only side effects are on
// the transaction handle it receives, because everything it did before the
// failing statement is rolled back and then done again. fn must not send mail,
// call HTTP APIs, publish events or mutate memory it does not reset.
func RetryTx(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	err := db.Transaction(fn)
	if err == nil || !IsStalePlan(err) {
		return err
	}
	if ctx := db.Statement.Context; ctx != nil && ctx.Err() != nil {
		record(Occurrence{Kind: Classify(err), Outcome: "skipped", Op: "tx", Err: err})
		return err
	}
	ResetStatements(db)
	counters.txReplays.Add(1)
	err2 := db.Transaction(fn)
	outcome(Classify(err), "tx", err2)
	return err2
}

// RetryEnvelope re-runs a host function that answers with a JSON envelope when
// that envelope reports a stale-plan failure, once, after the statement
// handles were dropped. It is the function-level safety net for host calls
// that run several statements inside a transaction of their own (the wasm
// data_mutate / data_batch imports), where the pool-level retry cannot restart
// the transaction because data statements already ran in it.
//
// Safe only for functions with this contract, which the wasm data imports
// have: (1) every error path rolls its transaction back before returning, so a
// failed attempt left nothing behind; (2) every effect outside the database
// (events, notifications) happens strictly AFTER commit, so a failed attempt
// emitted nothing; (3) host callbacks they invoke work on the transaction
// handle they are given. Do not use it for a function that does not guarantee
// all three.
func RetryEnvelope(db *gorm.DB, op string, fn func() []byte) []byte {
	out := fn()
	k := Classify(errors.New(string(out)))
	if k == KindNone || !isFailureEnvelope(out) {
		return out
	}
	ResetStatements(db)
	out2 := fn()
	if k2 := Classify(errors.New(string(out2))); k2 != KindNone && isFailureEnvelope(out2) {
		record(Occurrence{Kind: k, Outcome: "failed", Op: op, Err: errors.New("stale plan persisted after retry")})
	} else {
		record(Occurrence{Kind: k, Outcome: "recovered", Op: op})
	}
	return out2
}

func isFailureEnvelope(b []byte) bool {
	return bytes.Contains(b, []byte(`"success":false`))
}
