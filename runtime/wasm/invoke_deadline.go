package wasm

import (
	"context"
	"sync"
	"time"
)

// guestDeadline is the wall-clock budget of ONE guest invocation, with the
// time the guest spends blocked in outbound HTTP (http_fetch / http_request)
// taken OUT of it.
//
// Why. The budget (BackendSpec.TimeoutMs, 10 s by default) exists to stop a
// guest that loops or thrashes. It was a plain context deadline, so it also
// counted the seconds a guest waited for a third party: a PAC that takes 12 s
// to stamp a CFDI killed the guest AFTER the CFDI was stamped and BEFORE it
// was persisted — the fiscal document sat `pending` until a sweep recovered
// it minutes later, and the operator saw "sigue en curso" (QA 0922 ROL-FIS-01,
// FAC-F02; the same invocation took 20.8 s on another day). Each outbound
// call is already bounded by its own 30 s client timeout, so excluding it
// cannot let a guest run unbounded; hardCap is the absolute ceiling anyway.
type guestDeadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	until  time.Time
	hard   time.Time
	cancel context.CancelFunc
}

// withGuestDeadline derives the invocation context: cancelled when the budget
// (extended by excluded waits) or hardCap runs out, or when parent ends.
func withGuestDeadline(parent context.Context, budget, hardCap time.Duration) (context.Context, *guestDeadline, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	now := time.Now()
	d := &guestDeadline{until: now.Add(budget), hard: now.Add(hardCap), cancel: cancel}
	d.timer = time.AfterFunc(budget, cancel)
	return ctx, d, func() {
		d.timer.Stop()
		cancel()
	}
}

// excluding runs fn (a blocking outbound call) with the budget PAUSED: the
// clock stops while fn waits and resumes with what was left, never past the
// hard cap (which keeps counting).
func (d *guestDeadline) excluding(fn func()) {
	if d == nil {
		fn()
		return
	}
	d.mu.Lock()
	paused := d.timer.Stop()
	remaining := time.Until(d.until)
	d.mu.Unlock()
	if !paused {
		fn() // budget already spent: the invocation is being torn down
		return
	}
	fn()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.until = time.Now().Add(remaining)
	if d.until.After(d.hard) {
		d.until = d.hard
	}
	d.timer.Reset(time.Until(d.until))
}
