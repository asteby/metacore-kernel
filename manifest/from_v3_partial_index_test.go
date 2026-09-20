package manifest

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest/v3"
)

// A unique index with a where must not fold onto ColumnDef.Unique: that would
// create the GLOBAL unique index the predicate exists to avoid.
func TestFromV3PartialUniqueIndexNotFolded(t *testing.T) {
	m := &v3.Manifest{Models: []v3.Model{{
		Key: "Customer", Table: "customers",
		Columns: []v3.Column{{Name: "external_id", Type: "string"}, {Name: "code", Type: "string"}},
		Indices: []v3.Index{
			{Name: "customers_external_id_uq", Columns: []string{"external_id"}, Unique: true, Where: "external_id IS NOT NULL"},
			{Name: "customers_code_uq", Columns: []string{"code"}, Unique: true},
		},
	}}}
	def := FromV3(m).ModelDefinitions[0]
	for _, c := range def.Columns {
		if c.Name == "external_id" && c.Unique {
			t.Fatal("partial unique index folded onto ColumnDef.Unique (global unique index)")
		}
		if c.Name == "code" && !c.Unique {
			t.Fatal("plain single-column unique index must keep folding onto ColumnDef.Unique")
		}
	}
	if len(def.Indices) != 1 || def.Indices[0].Where != "external_id IS NOT NULL" || !def.Indices[0].Unique {
		t.Fatalf("partial index not carried: %+v", def.Indices)
	}
}
