package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// recorder is a compiled-tier handler that records every payload it receives.
type recorder struct {
	mu       sync.Mutex
	payloads [][]byte
	err      error
}

func (r *recorder) Handle(_ context.Context, _ uuid.UUID, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, append([]byte(nil), payload...))
	return r.err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.payloads)
}

func (r *recorder) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *recorder) payload(i int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.payloads[i]
}

// domainHarness boots a dispatcher whose only subscription is a compiled
// handler bound to `event`. Terminal deliveries land on `barrier`, the same
// serialisation the e2e tests use.
type domainHarness struct {
	bus     *events.Bus
	db      *gorm.DB
	rec     *recorder
	barrier chan dispatch.DeliveryResult
}

func newDomainHarness(t *testing.T, event string, opts ...dispatch.Option) *domainHarness {
	t.Helper()
	h := &domainHarness{
		bus:     events.NewBus(nil),
		db:      ledgerDB(t),
		rec:     &recorder{},
		barrier: make(chan dispatch.DeliveryResult, 8),
	}
	base := []dispatch.Option{
		dispatch.WithCompiledRegistry(dispatch.MapCompiledRegistry{"on_event": h.rec}),
		dispatch.WithLogger(silentLogger()),
		dispatch.WithRetryBackoff(0, 0),
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

// await blocks until n terminal deliveries have been observed.
func (h *domainHarness) await(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-h.barrier:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delivery %d/%d", i+1, n)
		}
	}
}

// settle gives the dispatcher a moment to prove it enqueued NOTHING. Used only
// by the negative assertions, where there is no terminal event to await.
func (h *domainHarness) settle() { time.Sleep(150 * time.Millisecond) }

// TestDomainEvent_IsRoutedByName is the regression this change exists for: a
// guest-emitted domain event carries no canonical model/action envelope and
// used to be silently dropped. It must now reach its subscriber.
func TestDomainEvent_IsRoutedByName(t *testing.T) {
	h := newDomainHarness(t, "pos.order_created")
	org := uuid.New()

	payload := map[string]any{"order_id": "o-1", "total": 250}
	n, err := h.bus.PublishWithCount(context.Background(), "kernel", "pos.order_created", org, payload)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n != 1 {
		t.Fatalf("subscriber count = %d, want 1 (the enqueued delivery)", n)
	}
	h.await(t, 1)

	if h.rec.count() != 1 {
		t.Fatalf("handler invocations = %d, want 1", h.rec.count())
	}
	var got map[string]any
	if err := json.Unmarshal(h.rec.payload(0), &got); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	if got["order_id"] != "o-1" {
		t.Fatalf("handler got %v, want the domain payload verbatim", got)
	}
	if row := singleDelivery(t, h.db); row.Event != "pos.order_created" {
		t.Fatalf("ledger event = %q, want pos.order_created", row.Event)
	}
}

// TestDomainEvent_RepeatedEmissionsEachDeliver guards the idempotency-key
// change: a domain event has no natural occurrence id, so two emissions must
// produce two deliveries rather than collapsing into one permanent no-op.
func TestDomainEvent_RepeatedEmissionsEachDeliver(t *testing.T) {
	h := newDomainHarness(t, "pos.order_created")
	org := uuid.New()

	for i := 0; i < 3; i++ {
		if err := h.bus.Publish(context.Background(), "kernel", "pos.order_created", org,
			map[string]any{"order_id": i}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	h.await(t, 3)

	if h.rec.count() != 3 {
		t.Fatalf("handler invocations = %d, want 3", h.rec.count())
	}
	if n := deliveryCount(t, h.db); n != 3 {
		t.Fatalf("ledger rows = %d, want 3", n)
	}
}

// TestEmittedNameWinsOverPayloadShape pins the mis-routing fix: a domain
// payload that happens to carry `model`/`action` keys used to be re-routed as
// `<addon>.<Model>.<Action>`, ignoring the name the emitter passed.
func TestEmittedNameWinsOverPayloadShape(t *testing.T) {
	h := newDomainHarness(t, "tires.tire_aging_alert")
	org := uuid.New()

	payload := map[string]any{
		"model":     "Product",
		"action":    "updated",
		"addon_key": "inventory",
		"tire_id":   "t-9",
	}
	if err := h.bus.Publish(context.Background(), "kernel", "tires.tire_aging_alert", org, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h.await(t, 1)

	if h.rec.count() != 1 {
		t.Fatalf("handler invocations = %d, want 1 (routed by emitted name)", h.rec.count())
	}
	if row := singleDelivery(t, h.db); row.Event != "tires.tire_aging_alert" {
		t.Fatalf("ledger event = %q, want the emitted name", row.Event)
	}
}

// TestEmittedNameWins_NoDeliveryToPayloadShapedSubscriber is the other half of
// the mis-routing fix: the subscriber the OLD reconstruction would have picked
// must now receive nothing.
func TestEmittedNameWins_NoDeliveryToPayloadShapedSubscriber(t *testing.T) {
	h := newDomainHarness(t, "inventory.Product.updated")
	org := uuid.New()

	if err := h.bus.Publish(context.Background(), "kernel", "tires.tire_aging_alert", org,
		map[string]any{"model": "Product", "action": "updated", "addon_key": "inventory"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h.settle()

	if h.rec.count() != 0 {
		t.Fatalf("handler invocations = %d, want 0 — the payload's shape must not route", h.rec.count())
	}
}

// TestCanonicalRouting_Unchanged is the no-regression guard for the live
// canonical subscriptions: routing by the bus-supplied name must resolve
// exactly as the old reconstruction did, including the natural-key idempotency
// that collapses a re-publish of the same row event.
func TestCanonicalRouting_Unchanged(t *testing.T) {
	h := newDomainHarness(t, "inventory.Transfer.created")
	org := uuid.New()

	publishCanonical(t, h.bus, "inventory", "Transfer", "created", org, "row-1")
	h.await(t, 1)
	if h.rec.count() != 1 {
		t.Fatalf("invocations = %d, want 1", h.rec.count())
	}

	// Same row + same event name = same occurrence: idempotent no-op.
	publishCanonical(t, h.bus, "inventory", "Transfer", "created", org, "row-1")
	h.settle()
	if h.rec.count() != 1 {
		t.Fatalf("invocations after re-publish = %d, want 1 (idempotent)", h.rec.count())
	}

	// A different row is a different occurrence.
	publishCanonical(t, h.bus, "inventory", "Transfer", "created", org, "row-2")
	h.await(t, 1)
	if h.rec.count() != 2 {
		t.Fatalf("invocations after second row = %d, want 2", h.rec.count())
	}
}

// TestCanonicalRouting_WildcardSubscription covers the `<addon>.*` shape the
// integration_github subscriptions use.
func TestCanonicalRouting_WildcardSubscription(t *testing.T) {
	h := newDomainHarness(t, "integration_github.*")
	org := uuid.New()

	publishCanonical(t, h.bus, "integration_github", "Issue", "created", org, "i-1")
	publishCanonical(t, h.bus, "integration_github", "Issue", "updated", org, "i-1")
	h.await(t, 2)

	if h.rec.count() != 2 {
		t.Fatalf("invocations = %d, want 2", h.rec.count())
	}
}

// TestSubscriberCount_ZeroWhenNothingRouted is the honest-count fix: the
// emitter used to always see 1 (the dispatcher's own wildcard tap) even when
// its event went nowhere.
func TestSubscriberCount_ZeroWhenNothingRouted(t *testing.T) {
	h := newDomainHarness(t, "pos.order_created")
	org := uuid.New()

	n, err := h.bus.PublishWithCount(context.Background(), "kernel", "nobody.listens", org,
		map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n != 0 {
		t.Fatalf("subscriber count = %d, want 0 — nothing was routed", n)
	}
	h.settle()
	if c := deliveryCount(t, h.db); c != 0 {
		t.Fatalf("ledger rows = %d, want 0", c)
	}
}

// TestRetryBackoff_SpacesAttempts is the backoff fix: a failing delivery must
// not re-enter the guest three times back to back.
func TestRetryBackoff_SpacesAttempts(t *testing.T) {
	h := newDomainHarness(t, "pos.order_created",
		dispatch.WithMaxAttempts(3),
		dispatch.WithRetryBackoff(40*time.Millisecond, time.Second),
	)
	h.rec.setErr(errors.New("trap"))

	start := time.Now()
	if err := h.bus.Publish(context.Background(), "kernel", "pos.order_created", uuid.New(),
		map[string]any{"order_id": "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case r := <-h.barrier:
		if r.Status != dispatch.StatusDead {
			t.Fatalf("status = %s, want dead", r.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not reach a terminal status")
	}

	// Waits before attempts 2 and 3 are ~40ms and ~80ms (minus up to 20%
	// jitter) — comfortably above the ~0 an immediate retry loop would take.
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("three attempts took %v — retries were not spaced", elapsed)
	}
	if h.rec.count() != 3 {
		t.Fatalf("attempts = %d, want 3", h.rec.count())
	}
}
