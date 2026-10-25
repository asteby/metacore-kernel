package dynamic

import (
	"context"
	"errors"
	"strings"
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

// splitBus mimics events.Bus for the retry contract: Publish runs the plain
// subscribers (plain++) and then the durable router; PublishDurableOnly runs the
// durable router alone. While down, the durable router fails with an error that
// reports (like events.DurableError) whether the siblings ran.
type splitBus struct {
	down                        atomic.Bool
	plain, durable              atomic.Int32
	fullCalls, durableOnlyCalls atomic.Int32
}

type siblingErr struct{ siblings bool }

func (e siblingErr) Error() string     { return "events: durable delivery failed: ledger down" }
func (e siblingErr) SiblingsRan() bool { return e.siblings }

func (b *splitBus) Publish(context.Context, string, string, uuid.UUID, any) error {
	b.fullCalls.Add(1)
	b.plain.Add(1)
	if b.down.Load() {
		return siblingErr{siblings: true}
	}
	b.durable.Add(1)
	return nil
}

func (b *splitBus) PublishDurableOnly(context.Context, string, string, uuid.UUID, any) error {
	b.durableOnlyCalls.Add(1)
	if b.down.Load() {
		return siblingErr{siblings: false}
	}
	b.durable.Add(1)
	return nil
}

func ageOutbox(svc *Service) {
	svc.db.Model(&EventOutbox{}).Where("1 = 1").Update("created_at", time.Now().UTC().Add(-time.Minute))
}

func makeDue(svc *Service) {
	svc.db.Model(&EventOutbox{}).Where("1 = 1").Update("next_attempt_at", time.Now().UTC().Add(-time.Second))
}

// The in-line attempt already ran the plain subscribers; the relay must retry
// the durable router only, so a flat subscriber runs exactly once.
func TestOutbox_RelayAfterLedgerFailureDoesNotRerunPlainSubscribers(t *testing.T) {
	bus := &splitBus{}
	bus.down.Store(true)
	svc := newOutboxTestService(t, bus)
	svc.publishCanonical(context.Background(), "dyn_items", "created", newUser(uuid.New()), uuid.NewString(), nil, map[string]any{"x": 1})

	var row EventOutbox
	if err := svc.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if !row.FannedOut || row.PublishedAt != nil {
		t.Fatalf("row after the failed in-line attempt: fanned_out=%v published=%v", row.FannedOut, row.PublishedAt)
	}

	ageOutbox(svc)
	svc.RelayOutboxOnce(context.Background()) // still down: durable-only, fails
	makeDue(svc)
	bus.down.Store(false)
	svc.RelayOutboxOnce(context.Background()) // healed: durable-only, records

	if bus.plain.Load() != 1 || bus.fullCalls.Load() != 1 {
		t.Fatalf("plain subscriber ran %d times (full publishes %d), want exactly 1", bus.plain.Load(), bus.fullCalls.Load())
	}
	if bus.durable.Load() != 1 || bus.durableOnlyCalls.Load() != 2 {
		t.Fatalf("durable recorded %d (durable-only calls %d), want 1 and 2", bus.durable.Load(), bus.durableOnlyCalls.Load())
	}
	if err := svc.db.First(&row, "id = ?", row.ID).Error; err != nil || row.PublishedAt == nil {
		t.Fatalf("row not published after the heal: %+v err=%v", row, err)
	}
}

// A row that was never marked (process died before the fan-out) still gets the
// full publication, plain subscribers included.
func TestOutbox_UnmarkedRowGetsFullPublication(t *testing.T) {
	bus := &splitBus{}
	svc := newOutboxTestService(t, bus)
	svc.db.Create(&EventOutbox{ID: uuid.New(), OrganizationID: uuid.New(), AddonKey: "kernel", EventName: "a.b.created",
		Payload: `{"x":1}`, CreatedAt: time.Now().UTC().Add(-time.Minute)})
	svc.RelayOutboxOnce(context.Background())
	if bus.fullCalls.Load() != 1 || bus.plain.Load() != 1 || bus.durableOnlyCalls.Load() != 0 {
		t.Fatalf("full=%d plain=%d durableOnly=%d", bus.fullCalls.Load(), bus.plain.Load(), bus.durableOnlyCalls.Load())
	}
}

// A relay attempt that is itself a full publication and fails in the ledger
// marks the row, so the NEXT attempt is durable-only.
func TestOutbox_RelayFullPublishFailureMarksRow(t *testing.T) {
	bus := &splitBus{}
	bus.down.Store(true)
	svc := newOutboxTestService(t, bus)
	svc.db.Create(&EventOutbox{ID: uuid.New(), OrganizationID: uuid.New(), AddonKey: "kernel", EventName: "a.b.created",
		Payload: `{"x":1}`, CreatedAt: time.Now().UTC().Add(-time.Minute)})
	svc.RelayOutboxOnce(context.Background())
	makeDue(svc)
	svc.RelayOutboxOnce(context.Background())
	if bus.plain.Load() != 1 || bus.durableOnlyCalls.Load() != 1 {
		t.Fatalf("plain=%d durableOnly=%d, want 1 and 1", bus.plain.Load(), bus.durableOnlyCalls.Load())
	}
}

func TestOutbox_RelayBacksOffExponentially(t *testing.T) {
	bus := &splitBus{}
	bus.down.Store(true)
	svc := newOutboxTestService(t, bus)
	svc.publishCanonical(context.Background(), "dyn_items", "created", newUser(uuid.New()), uuid.NewString(), nil, map[string]any{"x": 1})
	ageOutbox(svc)

	svc.RelayOutboxOnce(context.Background())
	var row EventOutbox
	svc.db.First(&row)
	if row.NextAttemptAt == nil || row.Attempts != 1 {
		t.Fatalf("after 1st attempt: %+v", row)
	}
	if d := time.Until(*row.NextAttemptAt); d < outboxBackoff(1)-5*time.Second || d > outboxBackoff(1) {
		t.Fatalf("next_attempt_at in %s, want about %s", d, outboxBackoff(1))
	}
	// Not due yet: a second sweep must not touch it.
	svc.RelayOutboxOnce(context.Background())
	if bus.durableOnlyCalls.Load() != 1 {
		t.Fatalf("row retried before its backoff elapsed (%d calls)", bus.durableOnlyCalls.Load())
	}
	if outboxBackoff(2) != 2*outboxBackoff(1) || outboxBackoff(1000) != outboxBackoffMax {
		t.Fatalf("backoff curve: %s %s %s", outboxBackoff(1), outboxBackoff(2), outboxBackoff(1000))
	}
}

func TestOutbox_ExhaustedRowIsReportedOnce(t *testing.T) {
	bus := &splitBus{}
	bus.down.Store(true)
	svc := newOutboxTestService(t, bus)
	var got []EventOutbox
	svc.onOutboxExhausted = func(_ context.Context, r EventOutbox) { got = append(got, r) }
	svc.publishCanonical(context.Background(), "dyn_items", "created", newUser(uuid.New()), uuid.NewString(), nil, map[string]any{"x": 1})
	ageOutbox(svc)
	svc.db.Model(&EventOutbox{}).Where("1 = 1").Update("attempts", outboxMaxAttempts-1)

	svc.RelayOutboxOnce(context.Background())
	makeDue(svc)
	svc.RelayOutboxOnce(context.Background()) // exhausted rows are not swept again

	if len(got) != 1 || got[0].Attempts != outboxMaxAttempts || got[0].LastError == "" {
		t.Fatalf("exhaustion reports = %+v", got)
	}
}

// A name no store can hold is rejected before persisting: no row, no attempts.
func TestOutbox_OversizedEventNameIsRejectedBeforePersisting(t *testing.T) {
	bus := &splitBus{}
	svc := newOutboxTestService(t, bus)
	long := strings.Repeat("x", MaxEventNameLen+1)
	pub := NewOutboxPublisher(svc.db, bus)
	pub.Publish(context.Background(), nil, []StagedEvent{
		{AddonKey: "kernel", Name: long, OrgID: uuid.New(), Payload: []byte(`{}`)},
		{AddonKey: "kernel", Name: "ok.event", OrgID: uuid.New(), Payload: []byte(`{}`)},
	})
	var rows []EventOutbox
	svc.db.Find(&rows)
	if len(rows) != 1 || rows[0].EventName != "ok.event" {
		t.Fatalf("rows = %+v, want only ok.event", rows)
	}
	if bus.fullCalls.Load() != 1 {
		t.Fatalf("publishes = %d, want 1 (the oversized one is rejected)", bus.fullCalls.Load())
	}
}

// The guest-event path marks fanned_out too.
func TestOutboxPublisher_LedgerFailureMarksFannedOut(t *testing.T) {
	bus := &splitBus{}
	bus.down.Store(true)
	svc := newOutboxTestService(t, bus)
	NewOutboxPublisher(svc.db, bus).Publish(context.Background(), nil, []StagedEvent{{
		AddonKey: "kernel", Name: "pos.order_created", OrgID: uuid.New(), Payload: []byte(`{"a":1}`)}})
	var row EventOutbox
	svc.db.First(&row)
	if !row.FannedOut || row.PublishedAt != nil {
		t.Fatalf("row = %+v", row)
	}
}
