package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// actionFieldReadonlyManifestJSON declares an action with an item_fields
// line-items column marked readonly alongside total — the exact shape a
// computed subtotal column uses (e.g. customers.create_sales_order).
const actionFieldReadonlyManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "orders", "name": "Orders", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Order",
      "table": "orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "create_order",
        "label": "Create order",
        "target_model": "Order",
        "handler": { "type": "wasm", "function": "handle_create_order" },
        "placement": "create",
        "fields": [
          {
            "key": "lines",
            "label": "Lines",
            "type": "array",
            "item_fields": [
              { "key": "qty", "label": "Qty", "type": "number" },
              { "key": "subtotal", "label": "Subtotal", "type": "number", "readonly": true, "total": true }
            ]
          }
        ]
      }
    ]
  }
}`

func TestFromV3_ActionFieldReadonly_ParsesAndProjects(t *testing.T) {
	m, err := v3.Parse([]byte(actionFieldReadonlyManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected readonly on an item_fields column: %v", err)
	}

	host := manifest.FromV3(m)
	orderActions := host.Actions["Order"]
	if len(orderActions) != 1 || len(orderActions[0].Fields) != 1 {
		t.Fatalf("unexpected host action shape: %+v", host.Actions)
	}
	itemFields := orderActions[0].Fields[0].ItemFields
	if len(itemFields) != 2 {
		t.Fatalf("expected 2 item_fields, got %d", len(itemFields))
	}
	subtotal := itemFields[1]
	if subtotal.Key != "subtotal" || !subtotal.Readonly || !subtotal.Total {
		t.Fatalf("subtotal column did not project Readonly/Total: %+v", subtotal)
	}
	qty := itemFields[0]
	if qty.Readonly {
		t.Fatalf("qty column should not be readonly: %+v", qty)
	}
}
