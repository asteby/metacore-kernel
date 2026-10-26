package dynamic

import (
	"context"
	"sync"
)

// CommitHook collects the work that must run only AFTER the caller's database
// transaction commits (today: the fan-out of events staged inside it).
//
//	ctx, hook := dynamic.WithCommitHook(ctx)
//	... run the action on tx (wasm InvokeInTx stages events into tx) ...
//	if err := tx.Commit().Error; err != nil { hook.Discard(); return err }
//	hook.Run(ctx)
//
// Not calling Run is safe: the staged events are already in
// kernel_event_outbox (committed) and the outbox relay publishes them.
type CommitHook struct {
	mu  sync.Mutex
	fns []func(context.Context)
}

type commitHookKey struct{}

// WithCommitHook returns ctx carrying a fresh CommitHook.
func WithCommitHook(ctx context.Context) (context.Context, *CommitHook) {
	h := &CommitHook{}
	return context.WithValue(ctx, commitHookKey{}, h), h
}

// CommitHookFrom returns the CommitHook on ctx, or nil.
func CommitHookFrom(ctx context.Context) *CommitHook {
	h, _ := ctx.Value(commitHookKey{}).(*CommitHook)
	return h
}

func (h *CommitHook) add(fn func(context.Context)) {
	h.mu.Lock()
	h.fns = append(h.fns, fn)
	h.mu.Unlock()
}

// Run executes the collected work, once, in registration order. Call it after
// tx.Commit() succeeded. ctx cancellation is ignored: the commit already
// happened, so the publication must not die with a request ctx.
func (h *CommitHook) Run(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	fns := h.fns
	h.fns = nil
	h.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	for _, fn := range fns {
		fn(ctx)
	}
}

// Discard drops the collected work (the transaction rolled back or failed to
// commit). The rows rolled back with it, so nothing is ever delivered.
func (h *CommitHook) Discard() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.fns = nil
	h.mu.Unlock()
}
