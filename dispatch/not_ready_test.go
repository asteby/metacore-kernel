package dispatch_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
)

type notReadyErr struct{}

func (notReadyErr) Error() string  { return `wasm: addon "customers" not loaded` }
func (notReadyErr) NotReady() bool { return true }

// flakyBoot answers "not loaded" for the first n calls (the boot reload
// window), then succeeds.
type flakyBoot struct {
	mu    sync.Mutex
	n     int
	calls int
}

func (f *flakyBoot) Handle(_ context.Context, _ uuid.UUID, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.n {
		return notReadyErr{}
	}
	return nil
}

// QA 0922 AUTO-F01: an event published while the subscriber's module is still
// loading must wait for it, not burn its 3 attempts in a second and die.
func TestNotReadySubscriberIsAwaitedNotDeadLettered(t *testing.T) {
	fb := &flakyBoot{n: 2}
	h := newDomainHarnessWith(t, "pos.order_created", fb,
		dispatch.WithMaxAttempts(3),
		dispatch.WithRetryBackoff(5*time.Millisecond, 20*time.Millisecond),
		dispatch.WithNotReadyWait(time.Minute),
	)
	if err := h.bus.Publish(context.Background(), "kernel", "pos.order_created", uuid.New(),
		map[string]any{"order_id": "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case r := <-h.barrier:
		if r.Status != dispatch.StatusDelivered {
			t.Fatalf("status = %s (%s), want delivered after the module came up", r.Status, r.Err)
		}
		if r.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1: waiting for a loading module must not spend attempts", r.Attempts)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("delivery did not reach a terminal status")
	}
}

// With the wait disabled the old behaviour holds: not-ready counts as failure.
func TestNotReadyWaitDisabledDeadLetters(t *testing.T) {
	fb := &flakyBoot{n: 100}
	h := newDomainHarnessWith(t, "pos.order_created", fb,
		dispatch.WithMaxAttempts(2),
		dispatch.WithRetryBackoff(0, 0),
		dispatch.WithNotReadyWait(0),
	)
	_ = h.bus.Publish(context.Background(), "kernel", "pos.order_created", uuid.New(), map[string]any{"order_id": "o-2"})
	select {
	case r := <-h.barrier:
		if r.Status != dispatch.StatusDead {
			t.Fatalf("status = %s, want dead", r.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no terminal status")
	}
}

func newDomainHarnessWith(t *testing.T, event string, handler dispatch.CompiledHandler, opts ...dispatch.Option) *domainHarness {
	t.Helper()
	h := &domainHarness{
		bus:     events.NewBus(nil),
		db:      ledgerDB(t),
		rec:     &recorder{},
		barrier: make(chan dispatch.DeliveryResult, 8),
	}
	base := []dispatch.Option{
		dispatch.WithCompiledRegistry(dispatch.MapCompiledRegistry{"on_event": handler}),
		dispatch.WithLogger(silentLogger()),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { h.barrier <- r }),
	}
	cancel, err := dispatch.Wire(h.bus, nil, h.db,
		staticProvider("customers", uuid.New(), event, "compiled", "on_event"),
		append(base, opts...)...)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(cancel)
	return h
}
