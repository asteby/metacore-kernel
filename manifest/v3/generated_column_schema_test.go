package v3

import (
	"encoding/json"
	"testing"
)

// A column may declare a `generated` arithmetic expression (Postgres STORED
// generated column). The strict v3 JSON Schema must ALLOW the property —
// before it was added, `additionalProperties:false` rejected any manifest
// using it ("additionalProperties 'generated' not allowed"), so the feature
// was unusable from a real addon manifest despite the Go type + DDL support.
func TestParse_GeneratedColumnAccepted(t *testing.T) {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Stock",
			"table": "stock",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "quantity", "type": "numeric", "not_null": true},
				map[string]interface{}{"name": "reserved", "type": "numeric", "not_null": true},
				map[string]interface{}{"name": "available", "type": "numeric", "generated": "quantity - reserved"},
			},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("Parse rejected a valid generated column: %v", err)
	}
}
