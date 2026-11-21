package v3

import "testing"

func importModel(spec map[string]interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key": "Product", "table": "products",
			"import": spec,
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "sku", "type": "text"},
				map[string]interface{}{"name": "name", "type": "text"},
			},
		},
	}
	return m
}

func TestImportSpec_Valid(t *testing.T) {
	m := importModel(map[string]interface{}{
		"sheet_name": "Productos", "max_rows": 5000,
		"instructions": []interface{}{"El SKU identifica la fila."},
		"columns": []interface{}{
			map[string]interface{}{"key": "sku", "header": "SKU", "aliases": []interface{}{"codigo"}, "required": true, "example": "LL-1"},
			map[string]interface{}{"key": "name", "header": "Nombre", "required": true},
		},
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("import spec should validate: %v", err)
	}
}

func TestImportSpec_Invalid(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"no columns":        {"sheet_name": "x"},
		"empty columns":     {"columns": []interface{}{}},
		"column no header":  {"columns": []interface{}{map[string]interface{}{"key": "sku"}}},
		"unknown prop":      {"columns": []interface{}{map[string]interface{}{"key": "sku", "header": "SKU", "natural": true}}},
		"unknown spec prop": {"columns": []interface{}{map[string]interface{}{"key": "sku", "header": "SKU"}}, "natural_key": "sku"},
	}
	for name, spec := range cases {
		if err := Validate(mustJSON(t, importModel(spec))); err == nil {
			t.Errorf("%s: want a schema error, got nil", name)
		}
	}
}
