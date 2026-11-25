package manifest

import "testing"

func TestProjectToolInputKeepsFullSchema(t *testing.T) {
	schema := map[string]interface{}{
		"type":     "object",
		"required": []interface{}{"status"},
		"properties": map[string]interface{}{
			"status": map[string]interface{}{"type": "string", "enum": []interface{}{"open", "closed"}, "description": "estado"},
			"limit":  map[string]interface{}{"type": "number", "default": 10},
			"tags":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
		},
	}
	full, params := projectToolInput(schema)
	if full["type"] != "object" {
		t.Fatalf("full schema lost: %v", full)
	}
	st := full["properties"].(map[string]any)["status"].(map[string]any)
	if enum, _ := st["enum"].([]any); len(enum) != 2 {
		t.Fatalf("enum dropped: %v", st)
	}
	if len(params) != 3 || params[0].Name != "limit" || params[1].Name != "status" || !params[1].Required || params[0].DefaultValue != "10" {
		t.Fatalf("params = %+v", params)
	}
	if f, p := projectToolInput(nil); f != nil || p != nil {
		t.Fatalf("empty schema must project to nothing")
	}
}
