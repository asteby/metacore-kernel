package v3

import (
	"strings"
	"testing"
)

func withProductExtension(columns []interface{}) map[string]interface{} {
	m := baseValid()
	m["extension_points"] = map[string]interface{}{
		"model_extensions_accepted": []interface{}{"Product"},
	}
	m["models"] = []interface{}{
		map[string]interface{}{
			"key": "Product", "table": "products",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "name", "type": "text"},
				map[string]interface{}{"name": "public_token", "type": "text"},
			},
		},
		map[string]interface{}{
			"key": "TireSpec", "table": "tire_specs", "extends": "inventory.Product",
			"columns": []interface{}{
				map[string]interface{}{"name": "width", "type": "integer"},
			},
		},
	}
	m["contributions"] = map[string]interface{}{
		"public_routes": []interface{}{
			map[string]interface{}{
				"key": "product_public", "model": "Product", "token_column": "public_token",
				"kind": "json", "columns": columns,
			},
		},
	}
	return m
}

func TestColumnCheck_AcceptsExtensionColumn(t *testing.T) {
	m := withProductExtension([]interface{}{"name", "TireSpec.width"})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("extension column on the owner route should validate: %v", err)
	}
}

func TestColumnCheck_RejectsUnknownExtensionColumn(t *testing.T) {
	m := withProductExtension([]interface{}{"TireSpec.nope"})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), `not a column of model "Product"`) {
		t.Fatalf("want unknown extension column refused, got %v", err)
	}
}
