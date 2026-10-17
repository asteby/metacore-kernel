package dispatch_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/google/uuid"
)

// TestEmitter_StampedFromBusNotPayload is the anti-spoofing guarantee: a guest
// ("evil") that writes {"emitter":"storefront_cart"} into its payload must be
// delivered as emitter "evil" — the key the bus was called with — and the
// ledger row records the same publisher.
func TestEmitter_StampedFromBusNotPayload(t *testing.T) {
	h := newDomainHarness(t, "storefront_cart.checkout_paid")
	org := uuid.New()

	raw := json.RawMessage(`{"session_id":"s-1","emitter":"storefront_cart"}`)
	if _, err := h.bus.PublishWithCount(context.Background(), "evil", "storefront_cart.checkout_paid", org, raw); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h.await(t, 1)

	var got map[string]any
	if err := json.Unmarshal(h.rec.payload(0), &got); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	if got[dispatch.EmitterField] != "evil" {
		t.Fatalf("emitter = %v, want evil (host-stamped, guest value overwritten)", got[dispatch.EmitterField])
	}
	if got["session_id"] != "s-1" {
		t.Fatalf("payload fields must survive, got %v", got)
	}
	if row := singleDelivery(t, h.db); row.Emitter != "evil" {
		t.Fatalf("ledger emitter = %q, want evil", row.Emitter)
	}
}

// TestEmitter_CanonicalEventCarriesPublisher: a canonical CRUD event the host
// publishes under the owner addon key reaches subscribers with that key as
// emitter, and a host ("kernel") publish is stamped "kernel".
func TestEmitter_CanonicalEventCarriesPublisher(t *testing.T) {
	h := newDomainHarness(t, "customers.Customer.created")
	org := uuid.New()
	payload := map[string]any{
		"id": "r-1", "occurrence_id": "occ-1", "model": "Customer", "action": "created", "addon_key": "customers",
	}
	if err := h.bus.Publish(context.Background(), "customers", "customers.Customer.created", org, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h.await(t, 1)
	var got map[string]any
	_ = json.Unmarshal(h.rec.payload(0), &got)
	if got[dispatch.EmitterField] != "customers" {
		t.Fatalf("emitter = %v, want customers", got[dispatch.EmitterField])
	}

	h2 := newDomainHarness(t, "pos.order_created")
	if err := h2.bus.Publish(context.Background(), "kernel", "pos.order_created", org, map[string]any{"x": 1}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h2.await(t, 1)
	got = nil
	_ = json.Unmarshal(h2.rec.payload(0), &got)
	if got[dispatch.EmitterField] != "kernel" {
		t.Fatalf("emitter = %v, want kernel", got[dispatch.EmitterField])
	}
}

// TestEmitter_SameOccurrenceStillCollapses: stamping the emitter must not
// change the idempotency key of a canonical event re-published by the outbox
// relay (same bytes, same publisher) — one delivery, not two.
func TestEmitter_SameOccurrenceStillCollapses(t *testing.T) {
	h := newDomainHarness(t, "customers.Customer.updated")
	org := uuid.New()
	raw := json.RawMessage(`{"id":"r-1","model":"Customer","action":"updated","addon_key":"customers","after":{"id":"r-1"}}`)
	for i := 0; i < 2; i++ {
		if err := h.bus.Publish(context.Background(), "customers", "customers.Customer.updated", org, raw); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	h.await(t, 1)
	h.settle()
	if n := deliveryCount(t, h.db); n != 1 {
		t.Fatalf("deliveries = %d, want 1 (replay collapses)", n)
	}
}
