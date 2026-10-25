package dynamic

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// flakyLedgerBus is a Publisher double with the contract of events.Bus plus the
// dispatcher registered as a durable router (events.SubscribeDurableRouting):
// while the ledger is "down" Publish returns the durable router's error. (The
// real bus cannot be imported here: events -> security -> bundle -> dynamic.
// events/durable_test.go covers the bus side of the contract.)
type flakyLedgerBus struct {
	down     atomic.Bool
	recorded atomic.Int32
}

func (f *flakyLedgerBus) Publish(context.Context, string, string, uuid.UUID, any) error {
	if f.down.Load() {
		return errors.New("events: durable delivery failed: ledger down")
	}
	f.recorded.Add(1)
	return nil
}

// A durable router that fails must leave the outbox row unpublished (the old
// behaviour stamped published_at over the loss); the relay publishes it once
// the ledger is back. A plain handler error must still NOT block the stamp.
func TestOutbox_DurableRouterFailureKeepsRowUnpublishedUntilRelay(t *testing.T) {
	router := &flakyLedgerBus{}
	router.down.Store(true)
	bus := router
	svc := newOutboxTestService(t, bus)

	svc.publishCanonical(context.Background(), "dyn_items", "created", newUser(uuid.New()), uuid.NewString(), nil, map[string]any{"x": 1})

	var row EventOutbox
	if err := svc.db.First(&row).Error; err != nil {
		t.Fatalf("outbox row missing: %v", err)
	}
	if row.PublishedAt != nil {
		t.Fatal("published_at stamped although the durable ledger write failed: event would be lost")
	}

	// Ledger heals; the relay (past its min-age guard) publishes the row.
	router.down.Store(false)
	svc.db.Model(&EventOutbox{}).Where("id = ?", row.ID).Update("created_at", time.Now().UTC().Add(-time.Minute))
	svc.RelayOutboxOnce(context.Background())

	if err := svc.db.First(&row, "id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.PublishedAt == nil {
		t.Fatal("relay did not stamp published_at after the ledger recovered")
	}
	if got := router.recorded.Load(); got != 1 {
		t.Fatalf("router recorded %d times, want 1", got)
	}
}

// While the ledger stays down the relay keeps the row (and counts attempts).
func TestOutbox_RelayKeepsRowWhileDurableRouterFails(t *testing.T) {
	router := &flakyLedgerBus{}
	router.down.Store(true)
	bus := router
	svc := newOutboxTestService(t, bus)
	svc.publishCanonical(context.Background(), "dyn_items", "created", newUser(uuid.New()), uuid.NewString(), nil, map[string]any{"x": 1})
	svc.db.Model(&EventOutbox{}).Where("1 = 1").Update("created_at", time.Now().UTC().Add(-time.Minute))

	svc.RelayOutboxOnce(context.Background())

	var row EventOutbox
	if err := svc.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.PublishedAt != nil || row.Attempts != 1 || row.LastError == "" {
		t.Fatalf("row should stay unpublished with attempts=1 and last_error: %+v", row)
	}
}

// Same guarantee on the guest-event path (OutboxPublisher.fanOut).
func TestOutboxPublisher_DurableRouterFailureKeepsRowUnpublished(t *testing.T) {
	router := &flakyLedgerBus{}
	router.down.Store(true)
	bus := router
	svc := newOutboxTestService(t, bus)
	pub := NewOutboxPublisher(svc.db, bus)
	pub.Publish(context.Background(), nil, []StagedEvent{{
		AddonKey: "kernel", Name: "pos.order_created", OrgID: uuid.New(), Payload: []byte(`{"a":1}`),
	}})
	var row EventOutbox
	if err := svc.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.PublishedAt != nil {
		t.Fatal("guest event stamped published_at over a failed ledger write")
	}
}
