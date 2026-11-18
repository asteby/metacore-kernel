package v3

import (
	"strings"
	"testing"
)

// withIngestAction builds a manifest with one action of the given extra keys.
func withIngestAction(extra map[string]interface{}) map[string]interface{} {
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
	a := map[string]interface{}{
		"key": "mark_paid", "target_model": "Invoice",
		"handler": map[string]interface{}{"type": "wasm", "function": "MarkPaid"},
	}
	for k, v := range extra {
		a[k] = v
	}
	m["contributions"] = map[string]interface{}{"actions": []interface{}{a}}
	return m
}

func TestActionIngest_Accepted(t *testing.T) {
	for name, extra := range map[string]map[string]interface{}{
		"plain":        {"ingest": true},
		"explicit off": {"ingest": false},
		"row":          {"ingest": true, "placement": "row"},
		"confirm only": {"ingest": true, "confirm": true},
	} {
		if err := Validate(mustJSON(t, withIngestAction(extra))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestActionIngest_ParsesFlag(t *testing.T) {
	m, err := Parse(mustJSON(t, withIngestAction(map[string]interface{}{"ingest": true})))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Contributions.Actions[0]; !got.Ingest {
		t.Fatalf("ingest not parsed: %+v", got)
	}
	m, err = Parse(mustJSON(t, withIngestAction(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Contributions.Actions[0].Ingest {
		t.Fatal("ingest must default to false")
	}
}

func TestActionIngest_RejectedCombinations(t *testing.T) {
	for name, tc := range map[string]struct {
		extra map[string]interface{}
		want  string
	}{
		"approval": {map[string]interface{}{"ingest": true,
			"approval": map[string]interface{}{"roles": []interface{}{"manager"}}}, "approval"},
		"supervisor_policy": {map[string]interface{}{"ingest": true, "supervisor_policy": "refund"}, "supervisor_policy"},
		"confirm_typed": {map[string]interface{}{"ingest": true,
			"confirm_typed": map[string]interface{}{"field": "id"}}, "confirm_typed"},
		"placement table":  {map[string]interface{}{"ingest": true, "placement": "table"}, "placement"},
		"placement create": {map[string]interface{}{"ingest": true, "placement": "create"}, "placement"},
	} {
		err := Validate(mustJSON(t, withIngestAction(tc.extra)))
		if err == nil {
			t.Fatalf("%s: ingest:true must be rejected", name)
		}
		if !strings.Contains(err.Error(), "ingest") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
	}
}

// The same combinations stay valid when ingest is not set: the flag is what
// makes them an error.
func TestActionIngest_CombinationsFineWithoutFlag(t *testing.T) {
	extra := map[string]interface{}{"supervisor_policy": "refund", "placement": "table"}
	if err := Validate(mustJSON(t, withIngestAction(extra))); err != nil {
		t.Fatal(err)
	}
}
