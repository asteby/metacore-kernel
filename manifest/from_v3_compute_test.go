package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const salesV3JSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "sales", "name": "Sales", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "SalesOrder",
      "table": "sales_orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "total", "type": "numeric" },
        { "name": "line_count", "type": "integer" }
      ],
      "relations": [
        {
          "name": "items",
          "kind": "one_to_many",
          "through": "SalesOrderItem",
          "foreign_key": "sales_order_id",
          "rollups": [
            { "target": "total", "fn": "sum", "from": "subtotal" },
            { "target": "line_count", "fn": "count" }
          ]
        }
      ]
    },
    {
      "key": "SalesOrderItem",
      "table": "sales_order_items",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "sales_order_id", "type": "uuid" },
        { "name": "quantity", "type": "integer" },
        { "name": "unit_price", "type": "numeric" },
        { "name": "discount", "type": "numeric" },
        { "name": "subtotal", "type": "numeric" }
      ],
      "formulas": [
        { "target": "subtotal", "expr": "quantity * unit_price - discount" }
      ]
    }
  ]
}`

// TestFromV3_CarriesRollupsAndFormulas proves the v3 -> legacy conversion
// preserves the compute-engine fields so the runtime (which reads the legacy
// shape) actually sees them. Without this, the engine would silently no-op.
func TestFromV3_CarriesRollupsAndFormulas(t *testing.T) {
	m, err := v3.Parse([]byte(salesV3JSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	legacy := manifest.FromV3(m)

	var parent, child *manifest.ModelDefinition
	for i := range legacy.ModelDefinitions {
		switch legacy.ModelDefinitions[i].ModelKey {
		case "SalesOrder":
			parent = &legacy.ModelDefinitions[i]
		case "SalesOrderItem":
			child = &legacy.ModelDefinitions[i]
		}
	}
	if parent == nil || child == nil {
		t.Fatalf("missing models after conversion")
	}

	// Tier-1 rollups carried onto the relation.
	if len(parent.Relations) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(parent.Relations))
	}
	rolls := parent.Relations[0].Rollups
	if len(rolls) != 2 {
		t.Fatalf("rollups not carried through: got %d, want 2", len(rolls))
	}
	if rolls[0].Target != "total" || rolls[0].Fn != "sum" || rolls[0].From != "subtotal" {
		t.Errorf("rollup[0] wrong: %+v", rolls[0])
	}
	if rolls[1].Target != "line_count" || rolls[1].Fn != "count" {
		t.Errorf("rollup[1] wrong: %+v", rolls[1])
	}

	// Tier-2 formulas carried onto the model.
	if len(child.Formulas) != 1 {
		t.Fatalf("formulas not carried through: got %d, want 1", len(child.Formulas))
	}
	if child.Formulas[0].Target != "subtotal" || child.Formulas[0].Expr != "quantity * unit_price - discount" {
		t.Errorf("formula wrong: %+v", child.Formulas[0])
	}

	// And the whole thing must pass the legacy/install validator.
	legacy.Kernel = ">=3.0.0 <4.0.0"
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("converted manifest failed legacy validate: %v", err)
	}
}
