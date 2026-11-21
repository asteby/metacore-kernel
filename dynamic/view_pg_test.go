package dynamic

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
)

// A view-backed model is created as a VIEW (no table, no indexes) and creating
// it twice is a no-op. Needs TEST_POSTGRES_DSN (skips otherwise).
func TestCreateTable_ViewModel(t *testing.T) {
	db, sfx := pgTestDB(t)
	src := "view_src_" + sfx
	view := "view_out_" + sfx
	mustExec(t, db, fmt.Sprintf(`CREATE TABLE %s (organization_id uuid, product_id uuid, qty int)`, src))
	t.Cleanup(func() { db.Exec("DROP VIEW IF EXISTS " + view); db.Exec("DROP TABLE IF EXISTS " + src) })
	org, prod := uuid.New(), uuid.New()
	mustExec(t, db, fmt.Sprintf(`INSERT INTO %s VALUES ('%s','%s',2),('%s','%s',3)`, src, org, prod, org, prod))

	def := manifest.ModelDefinition{
		TableName: view, ModelKey: "ViewOut", OrgScoped: true,
		View: fmt.Sprintf(`SELECT product_id AS id, organization_id, SUM(qty)::int AS qty FROM %s GROUP BY product_id, organization_id`, src),
	}
	if err := EnsureSchema(db, "view_addon", uuid.Nil, IsolationShared); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := CreateTable(db, "view_addon", uuid.Nil, IsolationShared, def); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if err := SyncSchema(db, "view_addon", uuid.Nil, IsolationShared, def); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	var sum int
	if err := db.Raw(fmt.Sprintf(`SELECT qty FROM %q.%q`, SchemaName("view_addon", uuid.Nil, IsolationShared), view)).Scan(&sum).Error; err != nil || sum != 5 {
		t.Fatalf("view sum=%d err=%v", sum, err)
	}
	var kind string
	db.Raw(`SELECT table_type FROM information_schema.tables WHERE table_name = ?`, view).Scan(&kind)
	if kind != "VIEW" {
		t.Fatalf("want VIEW, got %q", kind)
	}
	db.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, SchemaName("view_addon", uuid.Nil, IsolationShared)))
}
