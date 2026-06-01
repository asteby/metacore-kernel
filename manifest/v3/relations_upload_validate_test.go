package v3

import (
	"strings"
	"testing"
)

// modelWith returns a baseValid manifest carrying a single model whose
// relations slice is the provided value.
func modelWith(relations []interface{}) map[string]interface{} {
	m := baseValid()
	model := map[string]interface{}{
		"key":   "Customer",
		"table": "customers",
		"columns": []interface{}{
			map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
			map[string]interface{}{"name": "name", "type": "text", "not_null": true},
		},
	}
	if relations != nil {
		model["relations"] = relations
	}
	m["models"] = []interface{}{model}
	return m
}

func TestValidate_AcceptsModelRelations(t *testing.T) {
	m := modelWith([]interface{}{
		map[string]interface{}{
			"name": "vehicles", "kind": "one_to_many", "through": "Vehicle", "foreign_key": "customer_id",
		},
		map[string]interface{}{
			"name": "attachments", "kind": "one_to_many", "through": "Attachment", "foreign_key": "owner_id",
			"scope": map[string]interface{}{"owner_model": "Customer"},
		},
		map[string]interface{}{
			"name": "tags", "kind": "many_to_many", "through": "Tag", "foreign_key": "customer_id",
		},
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid manifest, got: %v", err)
	}
}

func TestValidate_RejectsUnknownRelationKind(t *testing.T) {
	m := modelWith([]interface{}{
		map[string]interface{}{
			"name": "vehicles", "kind": "has_many", "through": "Vehicle", "foreign_key": "customer_id",
		},
	})
	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatal("expected rejection for unknown relation kind")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error should mention kind, got: %v", err)
	}
}

func TestValidate_RejectsRelationMissingForeignKey(t *testing.T) {
	// Schema requires foreign_key, so this is caught at the schema layer.
	m := modelWith([]interface{}{
		map[string]interface{}{
			"name": "vehicles", "kind": "one_to_many", "through": "Vehicle",
		},
	})
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected rejection for relation missing foreign_key")
	}
}

func TestValidate_RejectsUnknownRelationField(t *testing.T) {
	// additionalProperties:false on ModelRelation must reject typos.
	m := modelWith([]interface{}{
		map[string]interface{}{
			"name": "vehicles", "kind": "one_to_many", "through": "Vehicle", "foreign_key": "customer_id",
			"bogus": "x",
		},
	})
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected rejection for unknown relation field (additionalProperties:false)")
	}
}

func TestValidate_AcceptsUploadActionField(t *testing.T) {
	m := baseValid()
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key":          "attach_file",
				"label":        "Adjuntar",
				"target_model": "Customer",
				"handler":      map[string]interface{}{"type": "webhook", "url": "https://example.com/a"},
				"fields": []interface{}{
					map[string]interface{}{
						"key": "file", "label": "Archivo", "type": "upload",
						"accept": "image/*,.pdf", "max_size": 10485760, "storage_path": "attachments/customers",
					},
				},
			},
		},
	}
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid manifest with upload field, got: %v", err)
	}
}

func TestValidate_RejectsUnknownUploadField(t *testing.T) {
	m := baseValid()
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key":          "attach_file",
				"target_model": "Customer",
				"handler":      map[string]interface{}{"type": "webhook", "url": "https://example.com/a"},
				"fields": []interface{}{
					map[string]interface{}{
						"key": "file", "type": "upload", "bogus_prop": "x",
					},
				},
			},
		},
	}
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected rejection for unknown action-field property (additionalProperties:false)")
	}
}
