package dispatch_test

import (
	"context"
	"errors"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/metadata"
	"strings"
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

	// Fail exactly the FIRST INSERT into the ledger (so "stop at the first error"
	// would skip the second subscription and the survivor assertion catches it).
	var inserts atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	if err := db.Callback().Create().Before("gorm:create").Register("test:fail_ledger", func(tx *gorm.DB) {
		if tx.Statement.Table != dispatch.DefaultTableName || !fail.Load() {
			return
		}
		if inserts.Add(1) == 1 {
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

// End to end with the REAL bus, dispatcher and outbox relay: when the ledger
// write fails, the plain subscriber (audit tap, bridges...) already ran once;
// the relay must retry only the durable router, so the plain subscriber is
// called EXACTLY once and the dispatcher delivers once.
func TestDurableBus_RelayAfterLedgerFailureDoesNotRerunPlainSubscribers(t *testing.T) {
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

	var plain atomic.Int32
	if err := bus.SubscribeTap("audit", "*", func(context.Context, uuid.UUID, any) error { plain.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := bus.Subscribe("print_bridge", event, func(context.Context, uuid.UUID, any) error { plain.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}

	svc := dynamic.New(dynamic.Config{DB: db, Bus: bus, Metadata: metadata.New(metadata.Config{CacheTTL: -1})})
	t.Cleanup(svc.StopOutboxRelay)
	svc.StopOutboxRelay()

	dynamic.NewOutboxPublisher(db, bus).Publish(context.Background(), nil, []dynamic.StagedEvent{{
		AddonKey: "kernel", Name: event, OrgID: uuid.New(), Payload: []byte(`{"order_id":"o-1"}`)}})
	if plain.Load() != 2 {
		t.Fatalf("plain subscribers after the in-line attempt = %d, want 2 (tap + bridge)", plain.Load())
	}
	var row dynamic.EventOutbox
	if err := db.First(&row).Error; err != nil || row.PublishedAt != nil || !row.FannedOut {
		t.Fatalf("outbox row = %+v err=%v, want unpublished and fanned_out", row, err)
	}

	age := func() {
		db.Model(&dynamic.EventOutbox{}).Where("1 = 1").Updates(map[string]any{
			"created_at": time.Now().UTC().Add(-time.Minute), "next_attempt_at": time.Now().UTC().Add(-time.Second)})
	}
	age()
	svc.RelayOutboxOnce(context.Background()) // ledger still down
	age()
	down.Store(false)
	svc.RelayOutboxOnce(context.Background()) // ledger back

	waitCalls(t, h, 1)
	time.Sleep(100 * time.Millisecond)
	if plain.Load() != 2 {
		t.Fatalf("plain subscribers ran %d times in total, want exactly 2 (once each): the relay re-executed them", plain.Load())
	}
	if n, c := deliveryCount(t, db), h.calls.Load(); n != 1 || c != 1 {
		t.Fatalf("ledger rows=%d dispatcher deliveries=%d, want 1/1", n, c)
	}
	if err := db.First(&row, "id = ?", row.ID).Error; err != nil || row.PublishedAt == nil || row.Attempts != 2 {
		t.Fatalf("row after recovery = %+v err=%v", row, err)
	}
}

// An event name the ledger cannot store is dropped loudly, not retried forever.
func TestDurableBus_OversizedEventNameIsNotAnError(t *testing.T) {
	db := ledgerDB(t)
	h := &countingHandler{}
	bus := wireWith(t, db, staticProvider("customers", uuid.New(), "x.y", "compiled", "on_event"), h)
	long := strings.Repeat("e", dynamic.MaxEventNameLen+1)
	if err := bus.Publish(context.Background(), "kernel", long, uuid.New(), map[string]any{"occurrence_id": "o"}); err != nil {
		t.Fatalf("a permanent, unrecordable event must not surface as retryable: %v", err)
	}
	if n := deliveryCount(t, db); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// The limit is inclusive: a name of exactly MaxEventNameLen is recorded.
func TestDurableBus_EventNameAtTheLimitIsRecorded(t *testing.T) {
	db := ledgerDB(t)
	h := &countingHandler{}
	name := strings.Repeat("e", dynamic.MaxEventNameLen)
	bus := wireWith(t, db, staticProvider("customers", uuid.New(), name, "compiled", "on_event"), h)
	if err := bus.Publish(context.Background(), "kernel", name, uuid.New(), map[string]any{"occurrence_id": "o"}); err != nil {
		t.Fatal(err)
	}
	if n := deliveryCount(t, db); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
}
