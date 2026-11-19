package v3

import (
	"strings"
	"testing"
)

func strictStageModel(transitions []interface{}, stages []interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key": "Order", "table": "orders",
			"stage_field": "status",
			"stages":      stages,
			"transitions": transitions,
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "status", "type": "text"},
				map[string]interface{}{"name": "total", "type": "numeric"},
				map[string]interface{}{"name": "paid", "type": "numeric"},
				map[string]interface{}{"name": "notes", "type": "text"},
			},
		},
	}
	return m
}

var policyStages = []interface{}{
	map[string]interface{}{"key": "draft"},
	map[string]interface{}{"key": "done", "is_final": true, "locked": true, "editable_when_locked": []interface{}{"notes"}},
}

func TestStagePolicy_SchemaAcceptsTransitionPolicy(t *testing.T) {
	m := strictStageModel([]interface{}{
		map[string]interface{}{
			"from": "draft", "to": "done",
			"guard": "paid >= total", "roles": []interface{}{"cashier"},
			"requires_fields": []interface{}{"notes"},
			"approval":        map[string]interface{}{"roles": []interface{}{"manager"}},
			"sla":             map[string]interface{}{"hours": 4, "on_breach": "notify"},
		},
	}, policyStages)
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("policy transition should validate: %v", err)
	}
}

func TestStagePolicy_RejectsBadPolicy(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"guard":  {"from": "draft", "to": "done", "guard": "nope >= 1"},
		"field":  {"from": "draft", "to": "done", "requires_fields": []interface{}{"ghost"}},
		"approv": {"from": "draft", "to": "done", "approval": map[string]interface{}{"roles": []interface{}{}}},
	}
	for name, tr := range cases {
		err := Validate(mustJSON(t, strictStageModel([]interface{}{tr}, policyStages)))
		if err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	// editable_when_locked sin locked, y con columna inexistente.
	bad := []interface{}{
		map[string]interface{}{"key": "draft", "editable_when_locked": []interface{}{"notes"}},
		map[string]interface{}{"key": "done"},
	}
	err := Validate(mustJSON(t, strictStageModel([]interface{}{}, bad)))
	if err == nil || !strings.Contains(err.Error(), "editable_when_locked requires locked") {
		t.Fatalf("want editable_when_locked requires locked, got %v", err)
	}
}

// P3 compat window: leaving a final stage without an approval block is a
// warning on install/upgrade and a hard error on publish.
func TestStagePolicy_FinalStageExitCompatWindow(t *testing.T) {
	raw := mustJSON(t, strictStageModel([]interface{}{
		map[string]interface{}{"from": "draft", "to": "done"},
		map[string]interface{}{"from": "done", "to": "draft"},
	}, policyStages))
	warns, err := ValidateWithOptions(raw, Options{})
	if err != nil {
		t.Fatalf("non-strict must tolerate: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "["+RuleFinalStageOutgoing+"]") {
		t.Fatalf("want one tagged warning, got %v", warns)
	}
	if _, err := ValidateWithOptions(raw, Options{Strict: true}); err == nil {
		t.Fatal("strict must refuse an exit from a final stage without approval")
	}
	// With approval it is the explicit reopen: clean in both modes.
	ok := mustJSON(t, strictStageModel([]interface{}{
		map[string]interface{}{"from": "draft", "to": "done"},
		map[string]interface{}{"from": "done", "to": "draft", "approval": map[string]interface{}{"roles": []interface{}{"manager"}}},
	}, policyStages))
	if w, err := ValidateWithOptions(ok, Options{Strict: true}); err != nil || len(w) != 0 {
		t.Fatalf("approved reopen: warns=%v err=%v", w, err)
	}
}
