package dyntest

import (
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// whenManifestJSON is the inventory side of the fulfillment split: it takes
// only the storable sale lines and leaves the service ones to whoever claims
// them.
const whenManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "inventory", "name": "Inventory", "version": "1.0.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "capabilities": [ { "kind": "event:subscribe", "target": "customers.SalesOrderItem.created" } ],
  "models": [
    {
      "key": "Stock",
      "table": "stock",
      "label": "Stock",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "qty", "type": "integer" }
      ]
    }
  ],
  "contributions": {
    "subscriptions": [
      {
        "event": "customers.SalesOrderItem.created",
        "handler": { "type": "wasm", "function": "on_sale_line" },
        "when": { "product_type": "storable" }
      },
      {
        "event": "customers.SalesOrderItem.created",
        "handler": { "type": "wasm", "function": "on_allocated_line" },
        "when": { "product_type": "storable" },
        "condition": { "addon_installed": "warehouse" }
      }
    ]
  }
}`

// TestSubscriptionWhenParses walks `when` + `condition` through v3.Parse.
func TestSubscriptionWhenParses(t *testing.T) {
	m, err := v3.Parse([]byte(whenManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with subscription when: %v", err)
	}
	subs := m.Contributions.Subscriptions
	if len(subs) != 2 {
		t.Fatalf("subscriptions = %d, want 2", len(subs))
	}
	if subs[0].When["product_type"] != "storable" {
		t.Errorf("when = %v, want product_type=storable", subs[0].When)
	}
	if subs[0].Condition != nil {
		t.Errorf("unconditional subscription got a condition: %+v", subs[0].Condition)
	}
	if subs[1].Condition == nil || subs[1].Condition.AddonInstalled != "warehouse" {
		t.Errorf("gated subscription condition = %+v", subs[1].Condition)
	}
}

// TestSubscriptionWithoutWhenStillParses is the back-compat guarantee for the
// subscriptions already in production.
func TestSubscriptionWithoutWhenStillParses(t *testing.T) {
	plain := strings.Replace(whenManifestJSON, `,
        "when": { "product_type": "storable" }
      },`, `
      },`, 1)
	m, err := v3.Parse([]byte(plain))
	if err != nil {
		t.Fatalf("v3.Parse rejected a subscription with no when: %v", err)
	}
	if len(m.Contributions.Subscriptions[0].When) != 0 {
		t.Errorf("when = %v, want empty", m.Contributions.Subscriptions[0].When)
	}
}

// TestSubscriptionWhenRejectedOnWildcard asserts a predicate on a wildcard
// pattern is refused: it would be evaluated against records of every model the
// pattern spans, where the field usually does not exist — a subscription that
// silently never fires.
func TestSubscriptionWhenRejectedOnWildcard(t *testing.T) {
	bad := strings.Replace(whenManifestJSON, `"event": "customers.SalesOrderItem.created",
        "handler": { "type": "wasm", "function": "on_sale_line" },`, `"event": "customers.*",
        "handler": { "type": "wasm", "function": "on_sale_line" },`, 1)
	_, err := v3.Parse([]byte(bad))
	if err == nil {
		t.Fatal("v3.Parse accepted a when predicate on a wildcard event")
	}
	if !strings.Contains(err.Error(), "wildcard event") {
		t.Errorf("unexpected error: %v", err)
	}
}
