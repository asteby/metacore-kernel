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

// A composite non-unique index without a where is carried on Indices (no
// Unique, no Where) so dynamic.EnsureDeclaredIndexes can build it; before,
// FromV3 dropped it and the declaration was inert.
func TestFromV3CompositeNonUniqueIndexCarried(t *testing.T) {
	m := &v3.Manifest{Models: []v3.Model{{
		Key: "Lead", Table: "crm_leads",
		Columns: []v3.Column{{Name: "customer_id", Type: "uuid"}, {Name: "status", Type: "string"}},
		Indices: []v3.Index{
			{Name: "crm_leads_org_customer_idx", Columns: []string{"organization_id", "customer_id"}},
			{Name: "crm_leads_status_idx", Columns: []string{"status"}},
		},
	}}}
	def := FromV3(m).ModelDefinitions[0]
	if len(def.Indices) != 1 {
		t.Fatalf("want only the composite on Indices, got %+v", def.Indices)
	}
	ix := def.Indices[0]
	if ix.Name != "crm_leads_org_customer_idx" || ix.Unique || ix.Where != "" ||
		len(ix.Columns) != 2 || ix.Columns[0] != "organization_id" || ix.Columns[1] != "customer_id" {
		t.Fatalf("composite non-unique index not carried as-is: %+v", ix)
	}
	for _, c := range def.Columns {
		if c.Name == "status" && !c.Index {
			t.Fatal("single-column index must keep folding onto ColumnDef.Index")
		}
		if c.Name == "customer_id" && (c.Index || c.Unique) {
			t.Fatal("a composite index must not fold onto a ColumnDef")
		}
	}
}
