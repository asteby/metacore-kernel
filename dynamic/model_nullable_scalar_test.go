package dynamic

import (
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// The "family of nullability": every OPTIONAL scalar column whose Go type is a
// non-pointer value (timestamp → time.Time, numeric → float64, bool → bool) has
// the same latent bug as the nullable uuid FK — its zero value is written
// verbatim instead of NULL, indistinguishable from "unset". So an OPTIONAL column
// WITHOUT a declared Default must be pointer-ized (*T) → an unset value persists
// NULL. A column WITH a Default keeps the VALUE form (the column DEFAULT covers
// "unset", preserving the addon's declared semantics). A REQUIRED column always
// keeps the value form (NOT NULL).
func TestColumnToField_NullableScalarFamily(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		elem reflect.Type
	}{
		{"timestamp", "timestamp", timeType},
		{"timestamptz", "timestamptz", timeType},
		{"datetime", "datetime", timeType},
		{"date", "date", timeType},
		{"numeric", "numeric", float64Type},
		{"decimal", "decimal", float64Type},
		{"float", "float", float64Type},
		{"double", "double", float64Type},
		{"boolean", "boolean", boolType},
		{"bool", "bool", boolType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// optional, no default → pointer
			opt, err := columnToField(manifest.ColumnDef{Name: "v", Type: tc.typ})
			if err != nil {
				t.Fatalf("columnToField(optional %s): %v", tc.typ, err)
			}
			if opt.Type.Kind() != reflect.Ptr || opt.Type.Elem() != tc.elem {
				t.Fatalf("optional %s = %v, want *%v", tc.typ, opt.Type, tc.elem)
			}

			// optional WITH default → value (column DEFAULT covers unset)
			var def any
			switch tc.elem {
			case boolType:
				def = false
			case float64Type:
				def = 0
			default:
				def = "now()"
			}
			withDef, err := columnToField(manifest.ColumnDef{Name: "v", Type: tc.typ, Default: def})
			if err != nil {
				t.Fatalf("columnToField(default %s): %v", tc.typ, err)
			}
			if withDef.Type != tc.elem {
				t.Fatalf("optional %s WITH default = %v, want value %v", tc.typ, withDef.Type, tc.elem)
			}

			// required → value (NOT NULL)
			req, err := columnToField(manifest.ColumnDef{Name: "v", Type: tc.typ, Required: true})
			if err != nil {
				t.Fatalf("columnToField(required %s): %v", tc.typ, err)
			}
			if req.Type != tc.elem {
				t.Fatalf("required %s = %v, want value %v", tc.typ, req.Type, tc.elem)
			}
		})
	}
}

// String/text/json/int/bigint columns must NOT be pointer-ized: empty string is a
// legitimate value, jsonb has its own handling, and ints carry no ambiguous NULL
// vs zero need here.
func TestColumnToField_NonPointerTypesUnchanged(t *testing.T) {
	for _, typ := range []string{"string", "text", "int", "bigint", "jsonb", "json"} {
		f, err := columnToField(manifest.ColumnDef{Name: "v", Type: typ})
		if err != nil {
			t.Fatalf("columnToField(%s): %v", typ, err)
		}
		if f.Type.Kind() == reflect.Ptr {
			t.Fatalf("%s must stay a value type, got pointer %v", typ, f.Type)
		}
	}
}

// coerceInputToStruct must recognize the pointer forms of the nullable scalar
// family: an empty value is dropped (pointer stays nil → NULL), a populated value
// is parsed/preserved so it unmarshals into the *T field.
func TestCoerce_NullableScalarPointers(t *testing.T) {
	st, err := BuildStructType(manifest.ModelDefinition{
		ModelKey:  "m",
		TableName: "things",
		Columns: []manifest.ColumnDef{
			{Name: "when", Type: "timestamptz"},
			{Name: "amount", Type: "numeric"},
			{Name: "active", Type: "bool"},
		},
	})
	if err != nil {
		t.Fatalf("BuildStructType: %v", err)
	}
	inst := reflect.New(st).Interface()

	// empty → all dropped → nil → NULL
	empty := map[string]any{"when": "", "amount": "", "active": ""}
	coerceInputToStruct(empty, inst)
	for _, k := range []string{"when", "amount", "active"} {
		if _, ok := empty[k]; ok {
			t.Fatalf("empty %q must be dropped so the pointer stays nil (NULL); input = %v", k, empty)
		}
	}

	// populated → parsed/preserved
	pop := map[string]any{"amount": "12.5", "active": "true", "when": "2026-07-18T00:00:00Z"}
	coerceInputToStruct(pop, inst)
	if pop["amount"] != 12.5 {
		t.Fatalf("*float64 amount = %v (%T), want 12.5 float64", pop["amount"], pop["amount"])
	}
	if pop["active"] != true {
		t.Fatalf("*bool active = %v (%T), want true bool", pop["active"], pop["active"])
	}
	if pop["when"] != "2026-07-18T00:00:00Z" {
		t.Fatalf("*time.Time when = %v, want preserved RFC3339 string", pop["when"])
	}

	// invalid numeric/bool → dropped
	bad := map[string]any{"amount": "not-a-number", "active": "maybe"}
	coerceInputToStruct(bad, inst)
	if _, ok := bad["amount"]; ok {
		t.Fatalf("invalid *float64 must be dropped; input = %v", bad)
	}
	if _, ok := bad["active"]; ok {
		t.Fatalf("invalid *bool must be dropped; input = %v", bad)
	}
}
