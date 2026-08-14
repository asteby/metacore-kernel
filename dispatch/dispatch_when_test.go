package dispatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// whenHarness boots a dispatcher with TWO subscriptions on the SAME event,
// split by disjoint `when` predicates — the fulfillment split this primitive
// exists for: inventory takes the storable sale lines, workshop the service
// ones, neither knowing the other exists.
type whenHarness struct {
	bus       *events.Bus
	db        *gorm.DB
	inventory *recorder
	workshop  *recorder
	barrier   chan dispatch.DeliveryResult
}

func newWhenHarness(t *testing.T, event string) *whenHarness {
	t.Helper()
	h := &whenHarness{
		bus:       events.NewBus(nil),
		db:        ledgerDB(t),
		inventory: &recorder{},
		workshop:  &recorder{},
		barrier:   make(chan dispatch.DeliveryResult, 8),
	}
	provider := dispatch.SubscriptionProviderFunc(func(context.Context, uuid.UUID) ([]dispatch.Subscription, error) {
		return []dispatch.Subscription{
			{
				AddonKey:     "inventory",
				Installation: uuid.New(),
				Event:        event,
				HandlerType:  "compiled",
				Function:     "on_storable",
				When:         map[string]string{"product_type": "storable"},
			},
			{
				AddonKey:     "workshop",
				Installation: uuid.New(),
				Event:        event,
				HandlerType:  "compiled",
				Function:     "on_service",
				When:         map[string]string{"product_type": "service"},
			},
		}, nil
	})
	cancel, err := dispatch.Wire(h.bus, nil, h.db, provider,
		dispatch.WithCompiledRegistry(dispatch.MapCompiledRegistry{
			"on_storable": h.inventory,
			"on_service":  h.workshop,
		}),
		dispatch.WithLogger(silentLogger()),
		dispatch.WithRetryBackoff(0, 0),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { h.barrier <- r }),
	)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(cancel)
	return h
}

func (h *whenHarness) publishLine(t *testing.T, org uuid.UUID, rowID, productType string) {
	t.Helper()
	payload := map[string]any{
		"id":        rowID,
		"model":     "SalesOrderItem",
		"action":    "created",
		"addon_key": "customers",
		"after":     map[string]any{"id": rowID, "product_type": productType, "qty": 2},
	}
	if err := h.bus.Publish(context.Background(), "kernel", "customers.SalesOrderItem.created", org, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func (h *whenHarness) await(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-h.barrier:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delivery %d/%d", i+1, n)
		}
	}
}

// TestWhen_SplitsOneEventBetweenAddons is the point of the primitive: one
// event, two subscribers, each woken only for the lines it owns.
func TestWhen_SplitsOneEventBetweenAddons(t *testing.T) {
	h := newWhenHarness(t, "customers.SalesOrderItem.created")
	org := uuid.New()

	h.publishLine(t, org, "line-1", "storable")
	h.await(t, 1)
	if got := h.inventory.count(); got != 1 {
		t.Errorf("inventory invocations = %d, want 1", got)
	}
	if got := h.workshop.count(); got != 0 {
		t.Errorf("workshop woke up for a storable line (%d invocations)", got)
	}

	h.publishLine(t, org, "line-2", "service")
	h.await(t, 1)
	if got := h.workshop.count(); got != 1 {
		t.Errorf("workshop invocations = %d, want 1", got)
	}
	if got := h.inventory.count(); got != 1 {
		t.Errorf("inventory woke up for a service line (now %d invocations)", got)
	}
}

// TestWhen_UnmatchedValueDeliversToNobody asserts a value no predicate claims
// is a clean no-op rather than a broadcast.
func TestWhen_UnmatchedValueDeliversToNobody(t *testing.T) {
	h := newWhenHarness(t, "customers.SalesOrderItem.created")
	org := uuid.New()

	n, err := h.bus.PublishWithCount(context.Background(), "kernel", "customers.SalesOrderItem.created", org, map[string]any{
		"id":     "line-3",
		"model":  "SalesOrderItem",
		"action": "created",
		"after":  map[string]any{"id": "line-3", "product_type": "consumable"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n != 0 {
		t.Errorf("enqueued %d deliveries, want 0", n)
	}
	time.Sleep(150 * time.Millisecond)
	if h.inventory.count() != 0 || h.workshop.count() != 0 {
		t.Errorf("a line no predicate claims was delivered (inventory=%d workshop=%d)", h.inventory.count(), h.workshop.count())
	}
	if got := deliveryCount(t, h.db); got != 0 {
		t.Errorf("ledger rows = %d, want 0", got)
	}
}

// TestWhen_MissingFieldNeverMatches covers the two payload shapes that carry no
// such field: a record without it, and a plain domain event with no record at
// all. Neither may satisfy a predicate.
func TestWhen_MissingFieldNeverMatches(t *testing.T) {
	h := newWhenHarness(t, "customers.SalesOrderItem.created")
	org := uuid.New()

	for _, payload := range []map[string]any{
		{"id": "line-4", "model": "SalesOrderItem", "action": "created", "after": map[string]any{"id": "line-4"}},
		{"order_id": "o-9", "total": 250},
	} {
		n, err := h.bus.PublishWithCount(context.Background(), "kernel", "customers.SalesOrderItem.created", org, payload)
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if n != 0 {
			t.Errorf("payload %v enqueued %d deliveries, want 0", payload, n)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if h.inventory.count() != 0 || h.workshop.count() != 0 {
		t.Errorf("a record without the field satisfied a predicate (inventory=%d workshop=%d)", h.inventory.count(), h.workshop.count())
	}
}

// TestWhen_DeleteMatchesAgainstBefore asserts a delete — whose `after` is empty
// — is evaluated against the state the row had before it was removed.
func TestWhen_DeleteMatchesAgainstBefore(t *testing.T) {
	h := newWhenHarness(t, "customers.SalesOrderItem.deleted")
	org := uuid.New()

	if err := h.bus.Publish(context.Background(), "kernel", "customers.SalesOrderItem.deleted", org, map[string]any{
		"id":     "line-5",
		"model":  "SalesOrderItem",
		"action": "deleted",
		"before": map[string]any{"id": "line-5", "product_type": "service"},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h.await(t, 1)
	if got := h.workshop.count(); got != 1 {
		t.Errorf("workshop invocations on delete = %d, want 1", got)
	}
	if got := h.inventory.count(); got != 0 {
		t.Errorf("inventory woke up for a deleted service line (%d)", got)
	}
}

// TestWhen_EmptyPredicateDeliversEverything is the back-compat guarantee: the
// eight subscriptions already in production declare no `when` and must keep
// receiving every occurrence.
func TestWhen_EmptyPredicateDeliversEverything(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrderItem.created")
	org := uuid.New()

	for _, pt := range []string{"storable", "service"} {
		if err := h.bus.Publish(context.Background(), "kernel", "customers.SalesOrderItem.created", org, map[string]any{
			"id":     "line-" + pt,
			"model":  "SalesOrderItem",
			"action": "created",
			"after":  map[string]any{"id": "line-" + pt, "product_type": pt},
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	h.await(t, 2)
	if got := h.rec.count(); got != 2 {
		t.Errorf("invocations = %d, want 2 (a subscription with no predicate takes everything)", got)
	}
}

// TestWhen_NonStringScalars covers the rendering rule: an integral number must
// not arrive as "5.000000", and a boolean matches "true".
func TestWhen_NonStringScalars(t *testing.T) {
	h := &whenHarness{
		bus:       events.NewBus(nil),
		db:        ledgerDB(t),
		inventory: &recorder{},
		workshop:  &recorder{},
		barrier:   make(chan dispatch.DeliveryResult, 8),
	}
	provider := dispatch.SubscriptionProviderFunc(func(context.Context, uuid.UUID) ([]dispatch.Subscription, error) {
		return []dispatch.Subscription{{
			AddonKey:     "inventory",
			Installation: uuid.New(),
			Event:        "customers.SalesOrderItem.created",
			HandlerType:  "compiled",
			Function:     "on_storable",
			When:         map[string]string{"qty": "5", "billable": "true"},
		}}, nil
	})
	cancel, err := dispatch.Wire(h.bus, nil, h.db, provider,
		dispatch.WithCompiledRegistry(dispatch.MapCompiledRegistry{"on_storable": h.inventory}),
		dispatch.WithLogger(silentLogger()),
		dispatch.WithRetryBackoff(0, 0),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { h.barrier <- r }),
	)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(cancel)

	if err := h.bus.Publish(context.Background(), "kernel", "customers.SalesOrderItem.created", uuid.New(), map[string]any{
		"id":     "line-6",
		"model":  "SalesOrderItem",
		"action": "created",
		"after":  map[string]any{"id": "line-6", "qty": 5, "billable": true},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	h.await(t, 1)
	if got := h.inventory.count(); got != 1 {
		t.Errorf("invocations = %d, want 1 (integral 5 and boolean true must match \"5\"/\"true\")", got)
	}
}
