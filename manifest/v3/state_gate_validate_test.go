package v3

import (
	"strings"
	"testing"
)

func withStageAction(requires []interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key": "Order", "table": "orders",
			"stage_field": "status",
			"stages": []interface{}{
				map[string]interface{}{"key": "draft", "label": "Draft"},
				map[string]interface{}{"key": "posted", "label": "Posted"},
			},
			"transitions": []interface{}{
				map[string]interface{}{"from": "draft", "to": "posted"},
			},
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "status", "type": "text"},
			},
		},
	}
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key": "post", "target_model": "Order", "requires_state": requires,
				"handler": map[string]interface{}{"type": "wasm", "function": "Post"},
			},
		},
	}
	return m
}

func TestRequiresState_MustBeDeclaredStage(t *testing.T) {
	if err := Validate(mustJSON(t, withStageAction([]interface{}{"draft"}))); err != nil {
		t.Fatalf("declared stage should validate: %v", err)
	}
	err := Validate(mustJSON(t, withStageAction([]interface{}{"void"})))
	if err == nil || !strings.Contains(err.Error(), `requires_state "void" is not a stage of model "Order"`) {
		t.Fatalf("want undeclared stage refused, got %v", err)
	}
}

func TestRequiresState_FreeFormWithoutMachine(t *testing.T) {
	m := withStageAction([]interface{}{"void"})
	mod := m["models"].([]interface{})[0].(map[string]interface{})
	delete(mod, "stage_field")
	delete(mod, "stages")
	delete(mod, "transitions")
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("without a stage machine requires_state stays free-form: %v", err)
	}
}

// Compat window (kernel #453 regression): a manifest published before the
// requires_state ⊂ stages rule existed must still pass in non-strict mode
// (install / upgrade) with a warning, and fail only in strict mode (publish).
func TestRequiresState_CompatWindow(t *testing.T) {
	raw := mustJSON(t, withStageAction([]interface{}{"void"}))

	// Upgrade / install: passes, with a tagged warning.
	warns, err := ValidateWithOptions(raw, Options{})
	if err != nil {
		t.Fatalf("non-strict must tolerate the legacy manifest: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "["+RuleRequiresStateInStages+"]") ||
		!strings.Contains(warns[0], `requires_state "void" is not a stage of model "Order"`) {
		t.Fatalf("want one tagged warning, got %v", warns)
	}
	if m, w, err := ParseWithOptions(raw, Options{}); err != nil || m == nil || len(w) != 1 {
		t.Fatalf("ParseWithOptions non-strict: m=%v warns=%v err=%v", m, w, err)
	}

	// Publish: hard error, and Validate / Parse keep their strict contract.
	if _, err := ValidateWithOptions(raw, Options{Strict: true}); err == nil ||
		!strings.Contains(err.Error(), `requires_state "void" is not a stage`) {
		t.Fatalf("strict must refuse, got %v", err)
	}
	if err := Validate(raw); err == nil {
		t.Fatal("Validate must stay strict")
	}

	// A clean manifest yields no warnings in either mode; other rules are
	// never downgraded.
	clean := mustJSON(t, withStageAction([]interface{}{"draft"}))
	if w, err := ValidateWithOptions(clean, Options{}); err != nil || len(w) != 0 {
		t.Fatalf("clean manifest: warns=%v err=%v", w, err)
	}
	if _, err := ValidateWithOptions([]byte(`{"apiVersion":"x"}`), Options{}); err == nil {
		t.Fatal("structural errors must never be downgraded")
	}
}
