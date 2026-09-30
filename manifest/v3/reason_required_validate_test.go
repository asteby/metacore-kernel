package v3

import (
	"strings"
	"testing"
)

func withReasonRequired(rr interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Sale",
			"table": "sales",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "status", "type": "text"},
			},
			"reason_required": rr,
		},
	}
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key": "cancel", "target_model": "Sale",
				"handler": map[string]interface{}{"type": "wasm", "function": "Cancel"},
			},
		},
	}
	return m
}

func TestReasonRequired_Valid(t *testing.T) {
	for _, rr := range []map[string]interface{}{
		{"delete": true},
		{"actions": []interface{}{"cancel"}, "min_length": 8},
		{"delete": true, "actions": []interface{}{"cancel"}},
	} {
		if err := Validate(mustJSON(t, withReasonRequired(rr))); err != nil {
			t.Fatalf("%v: expected valid, got %v", rr, err)
		}
	}
}

func TestReasonRequired_Rejections(t *testing.T) {
	cases := []struct {
		name string
		rr   map[string]interface{}
		want string
	}{
		{"empty policy", map[string]interface{}{}, "delete=true and/or"},
		{"unknown action", map[string]interface{}{"actions": []interface{}{"void"}}, `"void" is not an action targeting this model`},
		{"unknown key", map[string]interface{}{"delete": true, "on": "x"}, "reason_required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(mustJSON(t, withReasonRequired(c.rr)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestSettingScope(t *testing.T) {
	mk := func(scope string) map[string]interface{} {
		m := baseValid()
		st := map[string]interface{}{"key": "stock_policy", "type": "select", "options": []interface{}{map[string]interface{}{"value": "block", "label": "B"}}}
		if scope != "" {
			st["scope"] = scope
		}
		m["settings"] = []interface{}{st}
		return m
	}
	for _, ok := range []string{"", "org", "branch"} {
		if err := Validate(mustJSON(t, mk(ok))); err != nil {
			t.Fatalf("scope %q: %v", ok, err)
		}
	}
	if err := Validate(mustJSON(t, mk("region"))); err == nil {
		t.Fatal("scope region must be rejected")
	}
}
