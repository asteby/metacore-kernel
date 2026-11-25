package dynamic

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestIndexStatementsPartialUnique(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "customers",
		Columns:   []manifest.ColumnDef{{Name: "external_id", Type: "string"}},
		Indices: []manifest.IndexDef{
			{Name: "customers_external_id_uq", Columns: []string{"external_id"}, Unique: true, Where: "external_id IS NOT NULL"},
			{Name: "unsafe", Columns: []string{"external_id"}, Unique: true, Where: "1=1; DROP TABLE x"},
			{Name: "plain", Columns: []string{"external_id"}, Unique: true}, // no where: not emitted from Indices
		},
	}
	got := strings.Join(indexStatements("public", def, false), "\n")
	want := `CREATE UNIQUE INDEX IF NOT EXISTS "customers_external_id_uq" ON "public"."customers" ("external_id") WHERE "external_id" IS NOT NULL`
	if !strings.Contains(got, want) {
		t.Fatalf("missing partial index:\n%s", got)
	}
	if strings.Contains(got, "unsafe") || strings.Contains(got, `"plain"`) || strings.Contains(got, "DROP") {
		t.Fatalf("emitted an entry it must not:\n%s", got)
	}
}

// Composite indices without a where (non-unique ones now carried by FromV3)
// stay out of the transactional DDL plane: only EnsureDeclaredIndexes builds
// them, CONCURRENTLY. A plain CREATE INDEX here would block writes.
func TestIndexStatementsSkipCompositeNonUnique(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "crm_leads",
		Columns:   []manifest.ColumnDef{{Name: "customer_id", Type: "uuid"}},
		Indices: []manifest.IndexDef{
			{Name: "crm_leads_org_customer_idx", Columns: []string{"organization_id", "customer_id"}},
			{Name: "crm_leads_org_customer_uq", Columns: []string{"organization_id", "customer_id"}, Unique: true},
		},
	}
	for _, uniquePrefix := range []string{"uniq_", "idx_"} {
		got := strings.Join(indexStatementsWithPrefix("public", def, true, uniquePrefix), "\n")
		if strings.Contains(got, "crm_leads_org_customer") || strings.Contains(got, `"customer_id")`) {
			t.Fatalf("composite declared index leaked into the DDL plane:\n%s", got)
		}
	}
}
