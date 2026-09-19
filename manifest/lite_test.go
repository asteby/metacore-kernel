package manifest

import "testing"

func TestLite_DropsHeavyKeepsShellFields(t *testing.T) {
	m := Manifest{
		Key:     "customers",
		Name:    "Customers",
		Version: "1.0.0",
		Frontend: &FrontendSpec{
			Entry:  "remoteEntry.js",
			Format: "federation",
			Load:   "action",
		},
		Navigation: []NavGroup{{Title: "Sales"}},
		ModelDefinitions: []ModelDefinition{{
			ModelKey:  "SalesOrder",
			TableName: "sales_orders",
			Label:     "Order",
			Columns:   []ColumnDef{{Name: "id", Type: "uuid"}},
		}},
		Actions: map[string][]ActionDef{
			"SalesOrder": {{Key: "pay"}},
		},
		I18n: map[string]map[string]string{"es": {"a": "b"}},
		Backend: &BackendSpec{
			Runtime: "wasm",
			Entry:   "backend/backend.wasm",
			Exports: []string{"on_pay"},
		},
	}

	lite := Lite(m)
	if lite.Key != "customers" || lite.Frontend == nil || lite.Frontend.Load != "action" {
		t.Fatalf("shell identity/frontend lost: %+v", lite)
	}
	if len(lite.Navigation) != 1 {
		t.Fatalf("navigation dropped")
	}
	if len(lite.ModelDefinitions) != 1 || lite.ModelDefinitions[0].TableName != "sales_orders" {
		t.Fatalf("model index lost: %+v", lite.ModelDefinitions)
	}
	if len(lite.ModelDefinitions[0].Columns) != 0 {
		t.Fatalf("columns must be stripped in lite, got %d", len(lite.ModelDefinitions[0].Columns))
	}
	if lite.Actions != nil || lite.I18n != nil {
		t.Fatalf("heavy fields must be nil")
	}
	if lite.Backend == nil || lite.Backend.Runtime != "wasm" || len(lite.Backend.Exports) != 0 {
		t.Fatalf("backend must keep runtime/entry only: %+v", lite.Backend)
	}
	// Original untouched.
	if len(m.ModelDefinitions[0].Columns) != 1 {
		t.Fatal("Lite must not mutate the input")
	}
}
