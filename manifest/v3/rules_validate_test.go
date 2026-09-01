package v3

import "testing"

func baseWithSalesModel() map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Sale",
			"table": "sales",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
				map[string]interface{}{"name": "organization_id", "type": "uuid"},
				map[string]interface{}{"name": "amount_paid", "type": "numeric"},
				map[string]interface{}{"name": "amount_due", "type": "numeric"},
				map[string]interface{}{"name": "total", "type": "numeric"},
			},
		},
	}
	return m
}

func TestValidate_Rules_Valid(t *testing.T) {
	m := baseWithSalesModel()
	m["contributions"] = map[string]interface{}{
		"rules": []interface{}{
			map[string]interface{}{
				"key":   "finance_mismatch",
				"model": "Sale",
				"when":  "amount_paid + amount_due != total",
				"then": map[string]interface{}{
					"kind":     "flag",
					"severity": "warning",
					"message":  "customers.rules.finance_mismatch",
				},
			},
		},
	}
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid manifest, got: %v", err)
	}
}

func TestValidate_Rules_UnknownColumn(t *testing.T) {
	m := baseWithSalesModel()
	m["contributions"] = map[string]interface{}{
		"rules": []interface{}{
			map[string]interface{}{
				"key":   "bad",
				"model": "Sale",
				"when":  "nonexistent_column > 0",
				"then":  map[string]interface{}{"kind": "flag", "message": "x"},
			},
		},
	}
	err := Validate(mustJSON(t, m))
	if err == nil || !contains(err.Error(), "nonexistent_column") {
		t.Fatalf("expected unknown-column error, got: %v", err)
	}
}

func TestValidate_Rules_NotifyRequiresRoles(t *testing.T) {
	m := baseWithSalesModel()
	m["contributions"] = map[string]interface{}{
		"rules": []interface{}{
			map[string]interface{}{
				"key":   "needs_roles",
				"model": "Sale",
				"when":  "amount_due > 0",
				"then":  map[string]interface{}{"kind": "notify", "message": "x"},
			},
		},
	}
	err := Validate(mustJSON(t, m))
	if err == nil || !contains(err.Error(), "notify_roles") {
		t.Fatalf("expected notify_roles error, got: %v", err)
	}
}

func TestValidate_Rules_BadWhenSyntax(t *testing.T) {
	m := baseWithSalesModel()
	m["contributions"] = map[string]interface{}{
		"rules": []interface{}{
			map[string]interface{}{
				"key":   "bad_syntax",
				"model": "Sale",
				"when":  "amount_paid == 'draft'",
				"then":  map[string]interface{}{"kind": "flag", "message": "x"},
			},
		},
	}
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatalf("expected error for non-arithmetic when clause")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
