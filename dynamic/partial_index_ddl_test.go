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
