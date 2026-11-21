package nativekit

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// Op is one business operation: a database transaction plus the hooks that
// must only run once it commits (publish an event, enqueue a webhook, warm a
// cache) or that clean up if it rolls back. Modules create one per request via
// Run and pass it down; nothing outside the operation sees partial state.
type Op struct {
	ctx       context.Context
	tx        *gorm.DB
	afterOK   []func(context.Context)
	afterFail []func(context.Context)
	finalized bool
}

// NewOp binds an Op to an already open transaction owned by the caller (for
// example a request-scoped transaction). The caller MUST call Commit after its
// transaction commits, or Rollback if it aborts. Prefer Run when the module
// owns the transaction.
func NewOp(ctx context.Context, tx *gorm.DB) *Op {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Op{ctx: ctx, tx: tx}
}

// Context is the operation's context.
func (o *Op) Context() context.Context { return o.ctx }

// Tx is the transaction every write of the operation must go through.
func (o *Op) Tx() *gorm.DB { return o.tx }

// AfterCommit registers fn to run, in registration order, once the operation's
// transaction has committed. A panic in fn is recovered and logged: a failed
// side effect never turns a committed operation into an error.
func (o *Op) AfterCommit(fn func(ctx context.Context)) { o.afterOK = append(o.afterOK, fn) }

// OnRollback registers fn to run if the operation does not commit.
func (o *Op) OnRollback(fn func(ctx context.Context)) { o.afterFail = append(o.afterFail, fn) }

// Commit runs the AfterCommit hooks. Idempotent: hooks run at most once.
func (o *Op) Commit() { o.finish(o.afterOK) }

// Rollback runs the OnRollback hooks. Idempotent.
func (o *Op) Rollback() { o.finish(o.afterFail) }

func (o *Op) finish(hooks []func(context.Context)) {
	if o.finalized {
		return
	}
	o.finalized = true
	o.afterOK, o.afterFail = nil, nil
	for _, h := range hooks {
		runHook(o.ctx, h)
	}
}

func runHook(ctx context.Context, h func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("nativekit: post-commit hook panicked", "panic", r)
		}
	}()
	h(ctx)
}

// Run executes fn inside a transaction and fires the hooks according to the
// outcome. When db is already a transaction the work becomes a savepoint and
// the hooks fire when that savepoint releases, not at the outer commit: in that
// case own the outer transaction yourself and use NewOp + Commit.
func Run(ctx context.Context, db *gorm.DB, fn func(op *Op) error) error {
	var op *Op
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		op = NewOp(ctx, tx)
		return fn(op)
	})
	if op == nil { // BEGIN failed
		return err
	}
	if err != nil {
		op.Rollback()
		return err
	}
	op.Commit()
	return nil
}

// RunRetry is Run that retries the whole operation when the database aborts it
// for a transient reason (serialization failure or deadlock, SQLSTATE 40001 /
// 40P01). fn must be safe to re-run from scratch, which it is when all its
// writes go through op.Tx() and replay through Once. attempts < 1 means 3.
func RunRetry(ctx context.Context, db *gorm.DB, attempts int, fn func(op *Op) error) error {
	if attempts < 1 {
		attempts = 3
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = Run(ctx, db, fn); err == nil || !IsRetryable(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// IsRetryable reports whether err is a transient Postgres abort (serialization
// failure or deadlock) that a fresh attempt can resolve.
func IsRetryable(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "40001" || pg.Code == "40P01"
	}
	return false
}
