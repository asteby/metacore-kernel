package dispatch_test

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
)

// data.subscribers must not count the "*" taps of the dispatcher and the audit
// trail: only the org's subscriptions that match the event.
func TestEstimateSubscribers_WithDispatcherAndAuditWired(t *testing.T) {
	db := ledgerDB(t)
	bus := events.NewBus(nil)
	// audit.Wire subscribes exactly like this (its table needs Postgres, so
	// the tap is registered by hand here).
	if err := bus.SubscribeTap("kernel", "*", func(context.Context, uuid.UUID, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cancel, err := dispatch.Wire(bus, nil, db, staticProvider("customers", uuid.New(), "pos.order_created", "compiled", "x"),
		dispatch.WithLogger(silentLogger()))
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	org := uuid.New()
	if n := bus.EstimateSubscribers(context.Background(), org, "pos.order_created"); n != 1 {
		t.Fatalf("matching event: %d, want 1", n)
	}
	if n := bus.EstimateSubscribers(context.Background(), org, "pos.other"); n != 0 {
		t.Fatalf("unmatched event: %d, want 0 (taps are not subscribers)", n)
	}
}
