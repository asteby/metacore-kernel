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
