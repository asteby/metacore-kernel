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
	good = append(good, map[string]interface{}{"kind": "ref_state", "error_key": "sales.order_locked", "ref": "sales_order_id", "parent": "SalesOrder",
		"require": map[string]interface{}{"state": []interface{}{"draft", "sent"}}, "enforce": "always"})
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
		"enforce on sum_lte":    map[string]interface{}{"kind": "sum_lte", "error_key": "k", "ref": "sales_order_id", "parent": "SalesOrder", "sum": "subtotal", "max": "total", "enforce": "always"},
		"unknown enforce":       map[string]interface{}{"kind": "ref_state", "error_key": "k", "ref": "sales_order_id", "parent": "SalesOrder", "require": map[string]interface{}{"state": "open"}, "enforce": "sometimes"},
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

func TestValidate_UniqueRules(t *testing.T) {
	good := []interface{}{
		map[string]interface{}{"kind": "unique", "error_key": "sales.line_duplicated", "columns": []interface{}{"sales_order_id", "quantity"},
			"field": "quantity", "where": map[string]interface{}{"subtotal": []interface{}{1, 2}}},
		map[string]interface{}{"kind": "unique", "error_key": "sales.line_duplicated2", "columns": []interface{}{"sales_order_id"}},
	}
	if err := Validate(withRules(good...)); err != nil {
		t.Fatalf("valid unique rules rejected: %v", err)
	}

	bad := map[string]interface{}{
		"no columns":           map[string]interface{}{"kind": "unique", "error_key": "k"},
		"empty columns":        map[string]interface{}{"kind": "unique", "error_key": "k", "columns": []interface{}{}},
		"undeclared column":    map[string]interface{}{"kind": "unique", "error_key": "k", "columns": []interface{}{"nope"}},
		"repeated column":      map[string]interface{}{"kind": "unique", "error_key": "k", "columns": []interface{}{"quantity", "quantity"}},
		"field not in columns": map[string]interface{}{"kind": "unique", "error_key": "k", "columns": []interface{}{"quantity"}, "field": "subtotal"},
		"where undeclared":     map[string]interface{}{"kind": "unique", "error_key": "k", "columns": []interface{}{"quantity"}, "where": map[string]interface{}{"nope": true}},
		"with parent":          map[string]interface{}{"kind": "unique", "error_key": "k", "columns": []interface{}{"quantity"}, "ref": "sales_order_id", "parent": "SalesOrder"},
		"with enforce":         map[string]interface{}{"kind": "unique", "error_key": "k", "columns": []interface{}{"quantity"}, "enforce": "always"},
		"missing error_key":    map[string]interface{}{"kind": "unique", "columns": []interface{}{"quantity"}},
		"columns on ref_state": map[string]interface{}{"kind": "ref_state", "error_key": "k", "columns": []interface{}{"quantity"}},
	}
	for name, rule := range bad {
		if err := Validate(withRules(rule)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}
