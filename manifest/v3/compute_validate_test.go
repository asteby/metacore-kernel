package v3

import (
	"strings"
	"testing"
)

// salesModels returns the two-model (SalesOrder parent + SalesOrderItem child)
// models[] block exercising Tier-1 rollups and Tier-2 formulas, ready to be
// dropped onto a baseValid() manifest. mutate lets a test tweak it.
func salesModels() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"key":   "SalesOrder",
			"table": "sales_orders",
			"columns": []interface{}{
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "total", "type": "numeric"},
				map[string]interface{}{"name": "line_count", "type": "integer"},
			},
			"relations": []interface{}{
				map[string]interface{}{
					"name":        "items",
					"kind":        "one_to_many",
					"through":     "SalesOrderItem",
					"foreign_key": "sales_order_id",
					"rollups": []interface{}{
						map[string]interface{}{"target": "total", "fn": "sum", "from": "subtotal"},
						map[string]interface{}{"target": "line_count", "fn": "count"},
					},
				},
			},
		},
		map[string]interface{}{
			"key":   "SalesOrderItem",
			"table": "sales_order_items",
			"columns": []interface{}{
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "sales_order_id", "type": "uuid"},
				map[string]interface{}{"name": "quantity", "type": "integer"},
				map[string]interface{}{"name": "unit_price", "type": "numeric"},
				map[string]interface{}{"name": "discount", "type": "numeric"},
				map[string]interface{}{"name": "subtotal", "type": "numeric"},
			},
			"formulas": []interface{}{
				map[string]interface{}{"target": "subtotal", "expr": "quantity * unit_price - discount"},
			},
		},
	}
}

func TestValidate_RollupsAndFormulas_Valid(t *testing.T) {
	m := baseValid()
	m["models"] = salesModels()
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid manifest with rollups/formulas, got: %v", err)
	}
}

func TestValidate_Rollup_TargetNotOnParent(t *testing.T) {
	m := baseValid()
	models := salesModels()
	parent := models[0].(map[string]interface{})
	rel := parent["relations"].([]interface{})[0].(map[string]interface{})
	rel["rollups"].([]interface{})[0].(map[string]interface{})["target"] = "nonexistent_col"
	m["models"] = models
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "not a declared column on the parent") {
		t.Fatalf("expected parent-column error, got: %v", err)
	}
}

func TestValidate_Rollup_FromNotOnChild(t *testing.T) {
	m := baseValid()
	models := salesModels()
	parent := models[0].(map[string]interface{})
	rel := parent["relations"].([]interface{})[0].(map[string]interface{})
	rel["rollups"].([]interface{})[0].(map[string]interface{})["from"] = "ghost"
	m["models"] = models
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "not a declared column on the child") {
		t.Fatalf("expected child-column error, got: %v", err)
	}
}

func TestValidate_Rollup_BadFn(t *testing.T) {
	m := baseValid()
	models := salesModels()
	parent := models[0].(map[string]interface{})
	rel := parent["relations"].([]interface{})[0].(map[string]interface{})
	rel["rollups"].([]interface{})[0].(map[string]interface{})["fn"] = "median"
	m["models"] = models
	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatalf("expected bad-fn error, got nil")
	}
	// Rejected either by the schema enum or the cross-field validator.
	if !strings.Contains(err.Error(), "median") && !strings.Contains(err.Error(), "enum") && !strings.Contains(err.Error(), "fn") {
		t.Fatalf("expected fn error, got: %v", err)
	}
}

func TestValidate_Rollup_ExprInjection(t *testing.T) {
	m := baseValid()
	models := salesModels()
	parent := models[0].(map[string]interface{})
	rel := parent["relations"].([]interface{})[0].(map[string]interface{})
	r0 := rel["rollups"].([]interface{})[0].(map[string]interface{})
	delete(r0, "from")
	r0["expr"] = "subtotal); DROP TABLE sales_orders; --"
	m["models"] = models
	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatalf("expected injection rejection, got nil")
	}
}

func TestValidate_Formula_TargetNotOnModel(t *testing.T) {
	m := baseValid()
	models := salesModels()
	child := models[1].(map[string]interface{})
	child["formulas"].([]interface{})[0].(map[string]interface{})["target"] = "phantom"
	m["models"] = models
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "not a declared column on the model") {
		t.Fatalf("expected formula-target error, got: %v", err)
	}
}

func TestValidate_Formula_ExprSemicolon(t *testing.T) {
	m := baseValid()
	models := salesModels()
	child := models[1].(map[string]interface{})
	child["formulas"].([]interface{})[0].(map[string]interface{})["expr"] = "quantity; SELECT 1"
	m["models"] = models
	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatalf("expected expr-injection rejection, got nil")
	}
}

func TestValidate_Formula_ExprUnknownIdent(t *testing.T) {
	m := baseValid()
	models := salesModels()
	child := models[1].(map[string]interface{})
	child["formulas"].([]interface{})[0].(map[string]interface{})["expr"] = "quantity * mystery_price"
	m["models"] = models
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("expected unknown-column error, got: %v", err)
	}
}
