package staleplan

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"gorm.io/gorm"
)

// pool wraps the ConnPool GORM executes on. It is transparent unless a
// statement fails with a stale-plan error (see the package doc for the retry
// guarantee).
type pool struct {
	inner gorm.ConnPool
}

var (
	_ gorm.ConnPool         = (*pool)(nil)
	_ gorm.ConnPoolBeginner = (*pool)(nil)
	_ gorm.GetDBConnector   = (*pool)(nil)
)

func (p *pool) retryable(ctx context.Context, err error) (Kind, bool) {
	k := Classify(err)
	if k == KindNone {
		return KindNone, false
	}
	if ctx != nil && ctx.Err() != nil {
		record(Occurrence{Kind: k, Outcome: "skipped", Op: "ctx_done", Err: err})
		return k, false
	}
	return k, true
}

// reset drops the statement handles and scrubs the idle connections' server
// side statements so the retry re-prepares from the current catalog.
func (p *pool) reset(ctx context.Context) {
	generation.Add(1)
	resetStatements(p.inner)
	if db, err := p.GetDBConn(); err == nil {
		scrubIdle(ctx, db)
	}
}

func (p *pool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return p.inner.PrepareContext(ctx, query)
}

func (p *pool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	res, err := p.inner.ExecContext(ctx, query, args...)
	if err == nil {
		return res, nil
	}
	k, ok := p.retryable(ctx, err)
	if !ok {
		return res, err
	}
	p.reset(ctx)
	res, err2 := p.inner.ExecContext(ctx, query, args...)
	outcome(k, "exec", err2)
	return res, err2
}

func (p *pool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	rows, err := p.inner.QueryContext(ctx, query, args...)
	if err == nil {
		return rows, nil
	}
	k, ok := p.retryable(ctx, err)
	if !ok {
		return rows, err
	}
	p.reset(ctx)
	rows, err2 := p.inner.QueryContext(ctx, query, args...)
	outcome(k, "query", err2)
	return rows, err2
}

func (p *pool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	row := p.inner.QueryRowContext(ctx, query, args...)
	err := row.Err()
	if err == nil {
		return row
	}
	k, ok := p.retryable(ctx, err)
	if !ok {
		return row
	}
	p.reset(ctx)
	row = p.inner.QueryRowContext(ctx, query, args...)
	outcome(k, "query_row", row.Err())
	return row
}

func outcome(k Kind, op string, err error) {
	if err == nil {
		record(Occurrence{Kind: k, Outcome: "recovered", Op: op})
		return
	}
	record(Occurrence{Kind: k, Outcome: "failed", Op: op, Err: err})
}

// GetDBConn keeps db.DB() working through the wrapper.
func (p *pool) GetDBConn() (*sql.DB, error) {
	switch c := p.inner.(type) {
	case *sql.DB:
		return c, nil
	case gorm.GetDBConnector:
		return c.GetDBConn()
	}
	return nil, gorm.ErrInvalidDB
}

// Ping keeps the optional health probe gorm uses.
func (p *pool) Ping() error {
	if pg, ok := p.inner.(interface{ Ping() error }); ok {
		return pg.Ping()
	}
	return nil
}

func (p *pool) beginInner(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	switch b := p.inner.(type) {
	case gorm.TxBeginner:
		tx, err := b.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return tx, nil
	case gorm.ConnPoolBeginner:
		return b.BeginTx(ctx, opts)
	}
	return nil, gorm.ErrInvalidTransaction
}

// BeginTx implements gorm.ConnPoolBeginner.
func (p *pool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	in, err := p.beginInner(ctx, opts)
	if err != nil {
		return nil, err
	}
	if _, ok := in.(gorm.TxCommitter); !ok {
		return nil, gorm.ErrInvalidTransaction
	}
	return &txPool{p: p, ctx: ctx, opts: opts, inner: in, replayable: true}, nil
}

// sessionStmt is a statement that only configures the transaction (SET LOCAL
// search_path, set_config, ...). A transaction that has run nothing else can be
// restarted and these replayed to an identical state.
type sessionStmt struct {
	query string
	args  []interface{}
}

// isSessionSetup reports whether query only configures the session/transaction
// and reads or writes no table data.
func isSessionSetup(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	switch {
	case strings.HasPrefix(q, "set local "),
		strings.HasPrefix(q, "set session "),
		strings.HasPrefix(q, "set search_path"):
		return true
	case strings.HasPrefix(q, "select set_config("):
		return !strings.Contains(q, " from ")
	}
	return false
}

// txPool is a transaction that can be transparently restarted when a stale
// plan hits a statement and NOTHING but session setup (SET LOCAL ...) ran
// before it: the aborted transaction is rolled back, the setup is replayed on a
// fresh one, and the failed statement runs again. That is exactly the shape of
// the wasm data host (SET LOCAL search_path, then one query) and of gorm's
// default transaction around a single write.
type txPool struct {
	p    *pool
	ctx  context.Context
	opts *sql.TxOptions

	mu         sync.Mutex
	inner      gorm.ConnPool
	prefix     []sessionStmt // session-setup statements run so far
	replayable bool          // every statement so far was session setup
	prepared   bool          // PrepareContext handed out a *sql.Stmt bound to inner

	failedWasSetup bool // restart() scratch: the failing statement is session setup
}

var (
	_ gorm.ConnPool    = (*txPool)(nil)
	_ gorm.TxCommitter = (*txPool)(nil)
)

func (t *txPool) cur() gorm.ConnPool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inner
}

// begin registers the statement about to run and returns the live inner pool.
// first reports whether the transaction may still be restarted if it fails.
func (t *txPool) begin(query string, args []interface{}, isExec bool) (in gorm.ConnPool, restartable bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	restartable = t.replayable && !t.prepared
	if isExec && isSessionSetup(query) {
		t.prefix = append(t.prefix, sessionStmt{query: query, args: append([]interface{}(nil), args...)})
	} else {
		t.replayable = false
	}
	return t.inner, restartable
}

func (t *txPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	t.mu.Lock()
	t.prepared = true
	in := t.inner
	t.mu.Unlock()
	return in.PrepareContext(ctx, query)
}

// restart rolls the aborted transaction back, drops the stale handles, opens a
// replacement and replays the session setup. Only called while restartable.
func (t *txPool) restart(ctx context.Context) (gorm.ConnPool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.inner.(gorm.TxCommitter); ok {
		_ = c.Rollback()
	}
	t.p.reset(ctx)
	in, err := t.p.beginInner(t.ctx, t.opts)
	if err != nil {
		return nil, err
	}
	// The failing statement was appended to the prefix by begin() when it is
	// itself session setup; replaying it here would run it twice, so replay all
	// but a trailing statement that is about to be re-run by the caller.
	for _, st := range t.prefix[:len(t.prefix)-t.pendingSetup()] {
		if _, err := in.ExecContext(ctx, st.query, st.args...); err != nil {
			if c, ok := in.(gorm.TxCommitter); ok {
				_ = c.Rollback()
			}
			return nil, err
		}
	}
	t.inner = in
	return in, nil
}

// pendingSetup is 1 when the statement that just failed was itself recorded as
// session setup (the caller re-runs it), else 0. Called with t.mu held.
func (t *txPool) pendingSetup() int {
	if t.failedWasSetup {
		return 1
	}
	return 0
}

func (t *txPool) retry(ctx context.Context, restartable, wasSetup bool, err error, op string) (gorm.ConnPool, Kind, bool) {
	k := Classify(err)
	if k == KindNone {
		return nil, k, false
	}
	if !restartable || (ctx != nil && ctx.Err() != nil) {
		record(Occurrence{Kind: k, Outcome: "skipped", Op: "tx_later_stmt", Err: err})
		return nil, k, false
	}
	t.mu.Lock()
	t.failedWasSetup = wasSetup
	t.mu.Unlock()
	in, rerr := t.restart(ctx)
	if rerr != nil {
		record(Occurrence{Kind: k, Outcome: "failed", Op: op, Err: errors.Join(err, rerr)})
		return nil, k, false
	}
	return in, k, true
}

func (t *txPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	in, restartable := t.begin(query, args, true)
	res, err := in.ExecContext(ctx, query, args...)
	if err == nil {
		return res, nil
	}
	in2, k, ok := t.retry(ctx, restartable, isSessionSetup(query), err, "tx_stmt")
	if !ok {
		return res, err
	}
	res, err2 := in2.ExecContext(ctx, query, args...)
	outcome(k, "tx_stmt", err2)
	return res, err2
}

func (t *txPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	in, restartable := t.begin(query, args, false)
	rows, err := in.QueryContext(ctx, query, args...)
	if err == nil {
		return rows, nil
	}
	in2, k, ok := t.retry(ctx, restartable, false, err, "tx_stmt")
	if !ok {
		return rows, err
	}
	rows, err2 := in2.QueryContext(ctx, query, args...)
	outcome(k, "tx_stmt", err2)
	return rows, err2
}

func (t *txPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	in, restartable := t.begin(query, args, false)
	row := in.QueryRowContext(ctx, query, args...)
	err := row.Err()
	if err == nil {
		return row
	}
	in2, k, ok := t.retry(ctx, restartable, false, err, "tx_stmt")
	if !ok {
		return row
	}
	row = in2.QueryRowContext(ctx, query, args...)
	outcome(k, "tx_stmt", row.Err())
	return row
}

func (t *txPool) Commit() error {
	return t.cur().(gorm.TxCommitter).Commit()
}

func (t *txPool) Rollback() error {
	return t.cur().(gorm.TxCommitter).Rollback()
}

// StmtContext lets gorm's PreparedStmt-in-transaction path keep working when
// the wrapped transaction exposes it.
func (t *txPool) StmtContext(ctx context.Context, stmt *sql.Stmt) *sql.Stmt {
	in := t.cur()
	if s, ok := in.(interface {
		StmtContext(context.Context, *sql.Stmt) *sql.Stmt
	}); ok {
		t.mu.Lock()
		t.prepared = true
		t.mu.Unlock()
		return s.StmtContext(ctx, stmt)
	}
	return stmt
}

// GetDBConn: gorm resolves the *sql.DB of a transaction through this.
func (t *txPool) GetDBConn() (*sql.DB, error) { return t.p.GetDBConn() }
