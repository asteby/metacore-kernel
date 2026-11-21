package v3

import (
	"strings"
	"testing"
)

func withView(extra map[string]interface{}) map[string]interface{} {
	m := withLedger(extra)
	return m
}

func TestModelView_ValidAndParsed(t *testing.T) {
	m := withView(map[string]interface{}{"view": map[string]interface{}{"sql": "SELECT gen_random_uuid() AS id, organization_id, 1 AS amount, 'x' AS state FROM entries"}})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("view model should validate: %v", err)
	}
	parsed, err := Parse(mustJSON(t, m))
	if err != nil || parsed.Models[0].View == nil || parsed.Models[0].View.SQL == "" {
		t.Fatalf("view not parsed: %v", err)
	}
}

func TestModelView_Rejects(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"single statement": {"view": map[string]interface{}{"sql": "SELECT 1; DROP TABLE x"}},
		"must be a SELECT": {"view": map[string]interface{}{"sql": "DELETE FROM entries"}},
		"SQL comments":     {"view": map[string]interface{}{"sql": "SELECT 1 -- x"}},
		"is required":      {"view": map[string]interface{}{"sql": "  "}},
		"append_only":      {"append_only": true, "view": map[string]interface{}{"sql": "SELECT 1"}},
	}
	for want, extra := range cases {
		err := Validate(mustJSON(t, withView(extra)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	// id / organization_id are mandatory.
	m := baseValid()
	m["models"] = []interface{}{map[string]interface{}{
		"key": "Vw", "table": "vw",
		"columns": []interface{}{map[string]interface{}{"name": "nm", "type": "text"}},
		"view":    map[string]interface{}{"sql": "SELECT 'a' AS nm"},
	}}
	if err := Validate(mustJSON(t, m)); err == nil || !strings.Contains(err.Error(), "organization_id") {
		t.Errorf("missing org column: %v", err)
	}
}
