package dispatch_test

import (
	"context"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/google/uuid"
)

// qaResolver is a stand-in for the host model registry: it knows exactly one
// model, under both the spellings AddonService registers (the PascalCase
// ModelKey and the snake_case table name), and nothing else.
func qaResolver(_ context.Context, model string) string {
	switch model {
	case "SalesOrder", "sales_orders":
		return "SalesOrder"
	default:
		return "" // unknown to the registry
	}
}

type mismatchRecorder struct {
	mu   sync.Mutex
	hits []dispatch.ModelKeyMismatch
}

func (r *mismatchRecorder) record(m dispatch.ModelKeyMismatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits = append(r.hits, m)
}

func (r *mismatchRecorder) all() []dispatch.ModelKeyMismatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dispatch.ModelKeyMismatch, len(r.hits))
	copy(out, r.hits)
	return out
}

// TestModelKeyGuard_InertWithoutResolver pins the shape of the bug this guard
// exists for (ops#1435), and doubles as the proof that the guard is what fixes
// it: with no resolver wired the dispatcher routes `customers.sales_orders.updated`
// verbatim, it matches the `customers.SalesOrder.updated` subscription not at
// all, and NOTHING is recorded anywhere — no delivery, no error. That silence
// is precisely why the defect survived in production for as long as it did.
func TestModelKeyGuard_InertWithoutResolver(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()

	publishCanonical(t, h.bus, "customers", "sales_orders", "updated", org, "so-1")
	h.settle()

	if n := deliveryCount(t, h.db); n != 0 {
		t.Fatalf("without a resolver the mis-named event must route to nobody; got %d deliveries", n)
	}
}

// TestModelKeyGuard_RoutesMisnamedEventToCanonicalSubscriber is the guard doing
// its job: the same publication, with the host registry wired, reaches the
// subscriber registered under the manifest ModelKey.
func TestModelKeyGuard_RoutesMisnamedEventToCanonicalSubscriber(t *testing.T) {
	rec := &mismatchRecorder{}
	h := newDomainHarness(t, "customers.SalesOrder.updated",
		dispatch.WithModelKeyResolver(qaResolver),
		dispatch.WithOnModelKeyMismatch(rec.record))
	org := uuid.New()

	publishCanonical(t, h.bus, "customers", "sales_orders", "updated", org, "so-1")
	h.await(t, 1)

	if got := singleDelivery(t, h.db).Event; got != "customers.SalesOrder.updated" {
		t.Errorf("delivery ledger event: got %q, want the canonical name", got)
	}

	hits := rec.all()
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 mismatch report, got %d", len(hits))
	}
	// Every field the observer reports must be usable for grouping without
	// parsing prose — that is the whole point of reporting a struct.
	want := dispatch.ModelKeyMismatch{
		AddonKey:    "customers",
		RawModel:    "sales_orders",
		ResolvedKey: "SalesOrder",
		Action:      "updated",
		RawEvent:    "customers.sales_orders.updated",
		FixedEvent:  "customers.SalesOrder.updated",
	}
	if hits[0] != want {
		t.Errorf("mismatch report:\n got %+v\nwant %+v", hits[0], want)
	}
}

// TestModelKeyGuard_CanonicalNamePublishesClean is the false-positive guard: a
// publisher that already got the name right must deliver normally and must NOT
// be reported. A guard that cries on correct traffic gets muted, and then it
// stops guarding.
func TestModelKeyGuard_CanonicalNamePublishesClean(t *testing.T) {
	rec := &mismatchRecorder{}
	h := newDomainHarness(t, "customers.SalesOrder.updated",
		dispatch.WithModelKeyResolver(qaResolver),
		dispatch.WithOnModelKeyMismatch(rec.record))
	org := uuid.New()

	publishCanonical(t, h.bus, "customers", "SalesOrder", "updated", org, "so-1")
	h.await(t, 1)

	if n := len(rec.all()); n != 0 {
		t.Errorf("a correctly named publication must not be reported; got %d reports", n)
	}
}

// TestModelKeyGuard_UnknownModelIsLeftAlone pins the deliberate blind spot: a
// model the registry does not know (a core GORM table, a fixture) has no
// ModelKey to be measured against. It routes under the name it was given and
// is never reported — warning about it would drown the signal.
func TestModelKeyGuard_UnknownModelIsLeftAlone(t *testing.T) {
	rec := &mismatchRecorder{}
	h := newDomainHarness(t, "customers.widgets.updated",
		dispatch.WithModelKeyResolver(qaResolver),
		dispatch.WithOnModelKeyMismatch(rec.record))
	org := uuid.New()

	publishCanonical(t, h.bus, "customers", "widgets", "updated", org, "w-1")
	h.await(t, 1)

	if n := len(rec.all()); n != 0 {
		t.Errorf("unknown model must not be reported; got %d reports", n)
	}
}
