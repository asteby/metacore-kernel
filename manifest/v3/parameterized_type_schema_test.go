package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

// The v3 JSON Schema's column `type` must accept the Postgres-native
// parameterized forms numeric(p,s) / varchar(n) / vector(n) — the DDL plane
// (dynamic.pgColumnType / ValidateColumnType) materializes them, but the strict
// schema used to reject them at bundle-read, so a numeric(12,2) column was
// unusable from a real addon manifest despite full engine support.
func TestParse_ParameterizedColumnTypesAccepted(t *testing.T) {
	for _, ty := range []string{"numeric(12,2)", "varchar(40)", "vector(768)"} {
		m := baseValid()
		m["models"] = []interface{}{
			map[string]interface{}{
				"key":   "Item",
				"table": "items",
				"columns": []interface{}{
					map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
					map[string]interface{}{"name": "val", "type": ty},
				},
			},
		}
		raw, _ := json.Marshal(m)
		if _, err := Parse(raw); err != nil {
			t.Fatalf("Parse rejected parameterized type %q: %v", ty, err)
		}
	}
}

// A genuinely unknown type must still be rejected (the schema stays closed).
func TestParse_UnknownColumnTypeRejected(t *testing.T) {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Item",
			"table": "items",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
				map[string]interface{}{"name": "val", "type": "frobnicate"},
			},
		},
	}
	raw, _ := json.Marshal(m)
	if _, err := Parse(raw); err == nil {
		t.Fatal("Parse accepted an unknown column type 'frobnicate'")
	} else if !strings.Contains(err.Error(), "type") && !strings.Contains(err.Error(), "frobnicate") {
		// Not fatal — just ensure it failed for a type reason, best-effort.
		t.Logf("rejected as expected (message: %v)", err)
	}
}
