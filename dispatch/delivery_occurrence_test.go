package dispatch_test

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
)

// publishOccurrence publishes one canonical `updated` event for a row, with an
// explicit occurrence id and an `after` snapshot — the shape
// dynamic.publishCanonical puts on the bus. An empty occurrence id models the
// pre-occurrence_id kernel (and any host that builds the envelope by hand).
func publishOccurrence(t *testing.T, bus *events.Bus, orgID uuid.UUID, rowID, occurrenceID, status string) {
	t.Helper()
	payload := map[string]any{
		"id":        rowID,
		"model":     "SalesOrder",
		"action":    "updated",
		"addon_key": "customers",
		"after":     map[string]any{"id": rowID, "status": status},
	}
	if occurrenceID != "" {
		payload["occurrence_id"] = occurrenceID
	}
	if err := bus.Publish(context.Background(), "kernel", "customers.SalesOrder.updated", orgID, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// TestUpdate_EachOccurrenceIsItsOwnDelivery is the regression for the bug that
// froze the receivables ledger: `<Model>.updated` was keyed on the mutated ROW
// id, so the FIRST update of a row consumed the only delivery it would ever
// get and every later update was dropped as a phantom "re-publish". A credit
// sale moving credit_pending -> delivered never reached its settled handler.
//
// Two DIFFERENT updates of the same row must be two deliveries.
func TestUpdate_EachOccurrenceIsItsOwnDelivery(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()
	const row = "so-1"

	publishOccurrence(t, h.bus, org, row, uuid.NewString(), "credit_pending")
	h.await(t, 1)

	publishOccurrence(t, h.bus, org, row, uuid.NewString(), "delivered")
	h.await(t, 1)

	if got := h.rec.count(); got != 2 {
		t.Fatalf("invocations for two updates of the same row = %d, want 2", got)
	}
}

// TestUpdate_RedeliveryOfOneOccurrenceStaysSingle is the guard against the fix
// overshooting. The outbox relay re-publishes the PERSISTED payload bytes, so a
// replayed publication carries the SAME occurrence id — it must still collapse
// to one delivery. Losing this would turn a "never delivered" bug into a
// "delivered twice" one, which in an ERP that stamps CFDI is worse.
func TestUpdate_RedeliveryOfOneOccurrenceStaysSingle(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()
	const row = "so-2"
	occurrence := uuid.NewString()

	publishOccurrence(t, h.bus, org, row, occurrence, "delivered")
	h.await(t, 1)

	// Same occurrence, replayed verbatim by the relay.
	publishOccurrence(t, h.bus, org, row, occurrence, "delivered")
	h.settle()

	if got := h.rec.count(); got != 1 {
		t.Fatalf("invocations after replay of one occurrence = %d, want 1 (idempotent)", got)
	}
}

// TestUpdate_LegacyEnvelopeFallsBackToPayloadFingerprint covers the canonical
// events that carry no occurrence_id: an unpublished outbox row written by an
// older kernel, or a host assembling the envelope itself. The payload
// fingerprint has to keep BOTH properties — distinct updates deliver twice, a
// byte-identical replay only once.
func TestUpdate_LegacyEnvelopeFallsBackToPayloadFingerprint(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()
	const row = "so-3"

	publishOccurrence(t, h.bus, org, row, "", "credit_pending")
	h.await(t, 1)

	publishOccurrence(t, h.bus, org, row, "", "credit_pending") // verbatim replay
	h.settle()
	if got := h.rec.count(); got != 1 {
		t.Fatalf("invocations after identical legacy replay = %d, want 1", got)
	}

	publishOccurrence(t, h.bus, org, row, "", "delivered") // a real second update
	h.await(t, 1)
	if got := h.rec.count(); got != 2 {
		t.Fatalf("invocations after distinct legacy update = %d, want 2", got)
	}
}
