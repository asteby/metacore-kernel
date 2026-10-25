package dispatch_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func wireWith(t *testing.T, db *gorm.DB, provider dispatch.SubscriptionProvider, h dispatch.CompiledHandler) *events.Bus {
	t.Helper()
	bus := events.NewBus(nil)
	cancel, err := dispatch.Wire(bus, nil, db, provider,
		dispatch.WithCompiledRegistry(dispatch.MapCompiledRegistry{"on_event": h, "on_other": h}),
		dispatch.WithLogger(silentLogger()),
		dispatch.WithPollInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return bus
}

func waitCalls(t *testing.T, h *countingHandler, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("handler calls = %d, want %d", h.calls.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// SubscriptionsForOrg failing used to return (0, nil): the publisher believed
// the event was handled and stamped the outbox row. It must now be an error,
// and a retry with the same publication id must deliver exactly once.
func TestDurableBus_ProviderErrorIsPropagatedAndRetryDelivers(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"
	var down atomic.Bool
	down.Store(true)
	inner := staticProvider("customers", uuid.New(), event, "compiled", "on_event")
	provider := dispatch.SubscriptionProviderFunc(func(ctx context.Context, org uuid.UUID) ([]dispatch.Subscription, error) {
		if down.Load() {
			return nil, errors.New("db unavailable")
		}
		return inner.SubscriptionsForOrg(ctx, org)
	})
	h := &countingHandler{}
	bus := wireWith(t, db, provider, h)
	org := uuid.New()
	payload := map[string]any{"occurrence_id": "occ-1", "order_id": "o-1"}

	err := bus.Publish(context.Background(), "kernel", event, org, payload)
	if !errors.Is(err, events.ErrDurableDelivery) {
		t.Fatalf("Publish err = %v, want ErrDurableDelivery", err)
	}
	if n := deliveryCount(t, db); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}

	down.Store(false)
	if err := bus.Publish(context.Background(), "kernel", event, org, payload); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitCalls(t, h, 1)
	// A second retry is an idempotent no-op.
	if err := bus.Publish(context.Background(), "kernel", event, org, payload); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n, c := deliveryCount(t, db), h.calls.Load(); n != 1 || c != 1 {
		t.Fatalf("rows=%d calls=%d, want 1/1", n, c)
	}
}

// A failing ledger INSERT used to log and return false (no error). It must be
// an error; the retry inserts only the missing rows (stable delivery ids).
func TestDurableBus_LedgerInsertErrorIsPropagatedAndRetryCompletes(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"
	provider := dispatch.SubscriptionProviderFunc(func(context.Context, uuid.UUID) ([]dispatch.Subscription, error) {
		return []dispatch.Subscription{
			{AddonKey: "a", Installation: uuid.New(), Event: event, HandlerType: "compiled", Function: "on_event"},
			{AddonKey: "b", Installation: uuid.New(), Event: event, HandlerType: "compiled", Function: "on_other"},
		}, nil
	})
	h := &countingHandler{}
	bus := wireWith(t, db, provider, h)

	// Fail exactly the second INSERT into the ledger.
	var inserts atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	if err := db.Callback().Create().Before("gorm:create").Register("test:fail_ledger", func(tx *gorm.DB) {
		if tx.Statement.Table != dispatch.DefaultTableName || !fail.Load() {
			return
		}
		if inserts.Add(1) == 2 {
			_ = tx.AddError(errors.New("disk full"))
		}
	}); err != nil {
		t.Fatal(err)
	}

	org := uuid.New()
	payload := map[string]any{"occurrence_id": "occ-2"}
	err := bus.Publish(context.Background(), "kernel", event, org, payload)
	if !errors.Is(err, events.ErrDurableDelivery) {
		t.Fatalf("Publish err = %v, want ErrDurableDelivery", err)
	}
	waitCalls(t, h, 1)

	fail.Store(false)
	if err := bus.Publish(context.Background(), "kernel", event, org, payload); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitCalls(t, h, 2)
	time.Sleep(100 * time.Millisecond)
	if n, c := deliveryCount(t, db), h.calls.Load(); n != 2 || c != 2 {
		t.Fatalf("rows=%d calls=%d, want 2/2 (the surviving row must not run twice)", n, c)
	}
}
