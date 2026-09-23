package wasm

import (
	"context"
	"testing"
	"time"
)

// A guest blocked on a slow third party (a PAC stamping a CFDI) must not be
// killed by its own budget: the outbound wait is given back.
func TestGuestDeadlineExcludesOutboundWait(t *testing.T) {
	ctx, d, cancel := withGuestDeadline(context.Background(), 80*time.Millisecond, time.Second)
	defer cancel()
	d.excluding(func() { time.Sleep(200 * time.Millisecond) })
	if ctx.Err() != nil {
		t.Fatal("the outbound wait consumed the guest budget")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the remaining budget never ran out")
	}
}

// Guest CPU time still counts, and the hard cap bounds any extension.
func TestGuestDeadlineBudgetAndHardCap(t *testing.T) {
	ctx, _, cancel := withGuestDeadline(context.Background(), 50*time.Millisecond, time.Second)
	defer cancel()
	time.Sleep(120 * time.Millisecond)
	if ctx.Err() == nil {
		t.Fatal("a guest that does not wait on I/O must still hit its budget")
	}
	ctx2, d2, cancel2 := withGuestDeadline(context.Background(), 50*time.Millisecond, 100*time.Millisecond)
	defer cancel2()
	d2.excluding(func() { time.Sleep(90 * time.Millisecond) })
	time.Sleep(60 * time.Millisecond)
	if ctx2.Err() == nil {
		t.Fatal("the hard cap must bound the extension")
	}
}
