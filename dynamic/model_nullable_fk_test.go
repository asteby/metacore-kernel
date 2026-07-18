package dynamic

import (
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// A NULLABLE uuid column (an optional FK such as products.category_id) must be
// built as *uuid.UUID. As a non-pointer uuid.UUID its zero value is the nil UUID
// "00000000-…", which GORM writes on INSERT instead of NULL — violating the
// foreign key when the picker is left empty (SQLSTATE 23503). A REQUIRED uuid
// stays a value type (NOT NULL). This is the server-side guarantee: no client
// normalization can prevent a non-pointer field from persisting the nil UUID.
func TestColumnToField_OptionalUUIDIsPointer(t *testing.T) {
	optional, err := columnToField(manifest.ColumnDef{Name: "category_id", Type: "uuid", Ref: "categories"})
	if err != nil {
		t.Fatalf("columnToField(optional uuid): %v", err)
	}
	if optional.Type.Kind() != reflect.Ptr || optional.Type.Elem() != uuidType {
		t.Fatalf("optional uuid field type = %v, want *uuid.UUID", optional.Type)
	}

	required, err := columnToField(manifest.ColumnDef{Name: "owner_id", Type: "uuid", Ref: "users", Required: true})
	if err != nil {
		t.Fatalf("columnToField(required uuid): %v", err)
	}
	if required.Type != uuidType {
		t.Fatalf("required uuid field type = %v, want uuid.UUID (NOT NULL)", required.Type)
	}
}

// coerceInputToStruct must recognize the *uuid.UUID form: an empty value is
// dropped (leaving the pointer nil → NULL), an invalid one is dropped, a valid
// one is preserved for the unmarshal.
func TestCoerce_NullableUUIDPointer(t *testing.T) {
	st, err := BuildStructType(manifest.ModelDefinition{
		ModelKey:  "prod",
		TableName: "products",
		Columns:   []manifest.ColumnDef{{Name: "category_id", Type: "uuid", Ref: "categories"}},
	})
	if err != nil {
		t.Fatalf("BuildStructType: %v", err)
	}
	inst := reflect.New(st).Interface()

	in := map[string]any{"category_id": ""}
	coerceInputToStruct(in, inst)
	if _, ok := in["category_id"]; ok {
		t.Fatalf("empty *uuid.UUID must be dropped so the field stays nil (NULL); input = %v", in)
	}

	valid := map[string]any{"category_id": "11111111-1111-1111-1111-111111111111"}
	coerceInputToStruct(valid, inst)
	if valid["category_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("valid uuid must be preserved; got %v", valid["category_id"])
	}
}
