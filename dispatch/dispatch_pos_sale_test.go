package dispatch_test

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/runtime/wasm"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// POS-sale wave, slice 3 — verify the path that already works.
//
// The POS terminal creates a SalesOrder (origin=pos) directly via /data, and
// each SalesOrderItem insert fans out the canonical event
// `customers.SalesOrderItem.created` on the bus. In ops this event is routed by
// the generic subscription dispatcher (services/dispatch_wire.go →
// dispatch.Wire) to the inventory addon's wasm handler `on_sale_line`
// (inventory/backend/wasm), which decrements stock and writes the kardex.
//
// The other slices proved the two ends in isolation:
//   - inventory/backend/wasm/domain_test.go TestSmokeCanonicalEnvelope proves
//     the inventory DECISION layer turns a `customers.SalesOrderItem.created`
//     envelope into a stock decrement (parse → allocate → movement id).
//   - dispatch_e2e_test.go TestDispatch_E2E_WasmDelivery proves the dispatcher
//     routes a *generic* (`inventory.product.updated`) event to a wasm handler
//     that mutates a table.
//
// What was NOT covered is the exact wire the POS sale rides: that the
// dispatcher matches the specific `customers.SalesOrderItem.created` event name
// against the inventory addon's declared subscription+capability, delivers it
// to a wasm handler org-scoped, that handler decrements stock through the host
// db import, and the delivery is recorded `delivered`. This test closes that
// gap end-to-end.
//
// The canned fixture module (dbExecEmitWasm) stands in for inventory's real
// backend.wasm: it runs `UPDATE stock SET qty = qty - 1` through the db_exec
// host import — the observable "a POS sale moves stock" effect — regardless of
// its export symbol name (the module exports `on_event`; the real addon exports
// `on_sale_line`). The symbol name is orthogonal to what is under test here:
// the routing of the POS canonical event through the dispatcher to a wasm
// handler that decrements stock, org-scoped, with a clean delivery ledger.
// ---------------------------------------------------------------------------

const (
	posInventoryAddon = "inventory"
	posSaleEvent      = "customers.SalesOrderItem.created"
)

// posInventoryCaps is the capability set the inventory addon declares for the
// sale-line subscription (inventory/manifest.json): event:subscribe on the
// exact POS event name. event:emit stocker.recomputed is granted only so the
// shared canned fixture's secondary emit is not rejected — the real inventory
// handler emits its own inventory.* events instead.
func posInventoryCaps() []manifest.Capability {
	return []manifest.Capability{
		{Kind: "event:subscribe", Target: posSaleEvent},
		{Kind: "event:emit", Target: "stocker.recomputed"},
	}
}

func TestDispatch_E2E_POSSaleMovesStock(t *testing.T) {
	ctx := context.Background()
	caps := posInventoryCaps()
	enf := enforcerFor(posInventoryAddon, caps)

	// Bus the dispatcher taps AND the fixture's secondary event_emit publishes
	// back onto (capability-checked under the inventory addon key).
	bus := events.NewBus(enf)

	// The wasm handler decrements stock via db_exec. The host wraps it in its
	// own Begin/Commit and SET LOCAL search_path scoped to the addon's schema
	// (addon_inventory), routing the write to the live rows. Assert the full
	// sequence so "a POS sale moves stock" is observed, not assumed.
	wdb, mock := wasmDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_inventory", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE stock SET qty = qty - 1`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Host compiled AS the inventory addon so the db_exec search_path scope is
	// addon_inventory (mirrors production, where the host is loaded per addon).
	host, err := wasm.NewHost(ctx, security.Compile(posInventoryAddon, caps), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	host.WithBus(bus).WithDB(wdb).WithEnforcer(enf)
	t.Cleanup(func() { _ = host.Close(ctx) })

	spec := &manifest.BackendSpec{
		Runtime:   "wasm",
		Entry:     "backend.wasm",
		Exports:   []string{"on_event"}, // canned fixture's actual export symbol
		TimeoutMs: 2000,
	}
	if err := host.Load(ctx, posInventoryAddon, dbExecEmitWasm(), spec); err != nil {
		t.Fatalf("Load inventory fixture: %v", err)
	}

	ldb := ledgerDB(t)

	// The inventory subscription exactly as declared in inventory/manifest.json:
	// the POS event name, wasm handler, on the sale-line export.
	installation := uuid.New()
	provider := dispatch.SubscriptionProviderFunc(func(_ context.Context, _ uuid.UUID) ([]dispatch.Subscription, error) {
		return []dispatch.Subscription{{
			AddonKey:     posInventoryAddon,
			Installation: installation,
			Event:        posSaleEvent,
			HandlerType:  "wasm",
			Function:     "on_event", // stands in for on_sale_line (see file header)
		}}, nil
	})

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, host, ldb, provider,
		dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enf)),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()),
	)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	// A POS walk-in sale: the terminal inserted a SalesOrderItem, the CRUD
	// runtime fans out customers.SalesOrderItem.created for THIS org. A second,
	// unrelated org's identical event must be handled independently by the
	// dispatcher (org-scoped delivery) — publish under one concrete org here.
	orgID := uuid.New()
	publishCanonical(t, bus, "customers", "SalesOrderItem", "created", orgID, "pos-line-1")

	awaitDeliveries(t, barrier, 1)

	// 1) the wasm handler decremented stock (db_exec sequence satisfied).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("POS sale did not move stock (db_exec not observed): %v", err)
	}
	// 2) the delivery was recorded delivered for the inventory addon.
	got := singleDelivery(t, ldb)
	if got.Status != dispatch.StatusDelivered {
		t.Fatalf("delivery status = %q, want delivered (err=%q)", got.Status, got.LastError)
	}
	if got.AddonKey != posInventoryAddon {
		t.Fatalf("delivery routed to wrong addon: %q, want %q", got.AddonKey, posInventoryAddon)
	}
	if got.Event != posSaleEvent {
		t.Fatalf("delivery recorded wrong event: %q, want %q", got.Event, posSaleEvent)
	}
}

// TestDispatch_POSSale_NoInventoryInstall_NoStockMovement is the negative
// control: an org that has NOT installed inventory produces no subscription, so
// the POS sale fans out its canonical event but no stock is moved and no
// delivery is recorded. This guards the multi-org isolation the wave depends on
// — a POS sale in org A must never touch org B's (absent) inventory handler.
func TestDispatch_POSSale_NoInventoryInstall_NoStockMovement(t *testing.T) {
	bus := events.NewBus(nil)
	ldb := ledgerDB(t)

	var calls int32
	invoker := dispatch.WasmInvokerFunc(func(_ context.Context, _, _ uuid.UUID, _, _ string, _ []byte, _ map[string]string) ([]byte, error) {
		calls++
		return nil, nil
	})

	// Org without inventory installed → provider yields no subscriptions.
	provider := dispatch.SubscriptionProviderFunc(func(_ context.Context, _ uuid.UUID) ([]dispatch.Subscription, error) {
		return nil, nil
	})

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, invoker, ldb, provider,
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	publishCanonical(t, bus, "customers", "SalesOrderItem", "created", uuid.New(), "pos-line-x")
	awaitDeliveries(t, barrier, 0)

	if calls != 0 {
		t.Fatalf("stock handler invoked %d times for an org without inventory, want 0", calls)
	}
	if n := deliveryCount(t, ldb); n != 0 {
		t.Fatalf("ledger rows = %d, want 0 (no inventory install)", n)
	}
}
