package dynamic

import (
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestUUIDColumnsOf_ManifestStruct(t *testing.T) {
	st, err := BuildStructType(manifest.ModelDefinition{
		ModelKey:  "Stock",
		TableName: "stock",
		Columns: []manifest.ColumnDef{
			{Name: "product_id", Type: "uuid", Required: true},
			{Name: "variant_id", Type: "uuid"},
			{Name: "quantity", Type: "int", Required: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inst := reflect.New(st).Interface()
	got := uuidColumnsOf(inst)
	for _, name := range []string{"id", "product_id", "variant_id"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("missing %s in %#v", name, got)
		}
	}
	if _, ok := got["quantity"]; ok {
		t.Fatal("quantity is not a uuid")
	}
}
