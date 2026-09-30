package v3

import "testing"

func withSupervisorPolicy(policy string) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key": "Invoice", "table": "invoices",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
			},
		},
	}
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key": "cancel_fiscal", "target_model": "Invoice", "supervisor_policy": policy,
				"handler": map[string]interface{}{"type": "wasm", "function": "Cancel"},
			},
		},
	}
	return m
}

func TestSupervisorPolicy(t *testing.T) {
	for _, ok := range []string{"cancel_cfdi", "refund", "inventory_adjust", "discount"} {
		if err := Validate(mustJSON(t, withSupervisorPolicy(ok))); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"Cancel-CFDI", "1refund", "a b"} {
		if err := Validate(mustJSON(t, withSupervisorPolicy(bad))); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
