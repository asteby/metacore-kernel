package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

func withRules(rules ...interface{}) []byte {
	m := baseValid()
	models := salesModels()
	models[1].(map[string]interface{})["rules"] = rules
	m["models"] = models
	b, _ := json.Marshal(m)
	return b
}

func TestValidate_CrossRules(t *testing.T) {
	good := []interface{}{
		map[string]interface{}{"kind": "ref_state", "error_key": "sales.order_closed", "ref": "sales_order_id", "parent": "SalesOrder",
			"require": map[string]interface{}{"state": "open"}},
		map[string]interface{}{"kind": "sum_lte", "error_key": "sales.overpay", "ref": "sales_order_id", "parent": "SalesOrder",
			"sum": "subtotal", "max": "total", "where": map[string]interface{}{"quantity": []interface{}{1, 2}}},
	}
	good = append(good, map[string]interface{}{"kind": "sum_lte", "error_key": "sales.overpay2", "ref": "sales_order_id", "parent": "SalesOrder",
		"sum": "subtotal", "max": "total", "on_missing_parent": "skip"})
	if err := Validate(withRules(good...)); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}

	bad := map[string]interface{}{
		"unknown kind":          map[string]interface{}{"kind": "regex", "error_key": "k", "ref": "sales_order_id", "parent": "SalesOrder"},
		"ref not a column":      map[string]interface{}{"kind": "ref_state", "error_key": "k", "ref": "nope", "parent": "SalesOrder", "require": map[string]interface{}{"state": "open"}},
		"ref_state w/o require": map[string]interface{}{"kind": "ref_state", "error_key": "k", "ref": "sales_order_id", "parent": "SalesOrder"},
		"sum_lte bad sum col":   map[string]interface{}{"kind": "sum_lte", "error_key": "k", "ref": "sales_order_id", "parent": "SalesOrder", "sum": "nope", "max": "total"},
		"injection in require":  map[string]interface{}{"kind": "ref_state", "error_key": "k", "ref": "sales_order_id", "parent": "SalesOrder", "require": map[string]interface{}{"state; DROP TABLE x": "open"}},
		"bad on_missing_parent": map[string]interface{}{"kind": "sum_lte", "error_key": "k", "ref": "sales_order_id", "parent": "SalesOrder", "sum": "subtotal", "max": "total", "on_missing_parent": "ignore"},
		"missing error_key":     map[string]interface{}{"kind": "ref_state", "ref": "sales_order_id", "parent": "SalesOrder", "require": map[string]interface{}{"state": "open"}},
	}
	for name, rule := range bad {
		if err := Validate(withRules(rule)); err == nil {
			t.Errorf("%s: expected rejection", name)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: empty error", name)
		}
	}
}
