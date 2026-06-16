package dynamic

import (
	"context"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
)

// When Create is invoked with the table/route name (the action-create path
// passes "transfers", not the ModelKey "Transfer"), the canonical event must
// still fire on `<addon>.<ModelKey>.<action>` so subscribers — which register
// on the ModelKey form — match. Regression for stock never moving because the
// transfer_stock create emitted `inventory.transfers.created` while the wasm
// handler was subscribed to `inventory.Transfer.created`.
func TestCanonicalEventUsesModelKeyNotTable(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})

	bus := newFanOutBus()
	const addonKey = "shop"

	var (
		mu      sync.Mutex
		hitsKey int // events delivered on the ModelKey form
	)
	bus.Subscribe(addonKey+".Product.created", func(_ context.Context, _ uuid.UUID, _ any) error {
		mu.Lock()
		hitsKey++
		mu.Unlock()
		return nil
	})

	svc := New(Config{
		DB:               db,
		Metadata:         meta,
		Bus:              bus,
		AddonKeyForModel: func(_ context.Context, _ string) string { return addonKey },
		// table "test_products" -> ModelKey "Product"
		ModelKeyForModel: func(_ context.Context, model string) string {
			if model == "test_products" {
				return "Product"
			}
			return ""
		},
	})
	user := newUser(uuid.New())

	// Create is called with the TABLE name, as the action-create path does.
	createProduct(t, svc, user, "Widget", 9.99)

	mu.Lock()
	defer mu.Unlock()
	if hitsKey != 1 {
		t.Fatalf("subscriber on %s.Product.created got %d events, want 1 (event was published on the table name, not the ModelKey)", addonKey, hitsKey)
	}
	// And the published event name + payload Model are the ModelKey form.
	var found bool
	for _, p := range bus.published {
		if p.event == addonKey+".Product.created" {
			found = true
			if ev, ok := p.payload.(*CanonicalEvent); ok && ev.Model != "Product" {
				t.Fatalf("payload.Model = %q, want Product", ev.Model)
			}
		}
		if p.event == addonKey+".test_products.created" {
			t.Fatalf("event published on the table name %q — should be canonicalized to the ModelKey", p.event)
		}
	}
	if !found {
		t.Fatal("no event published on the ModelKey form")
	}
}
