package dynamic

// Regression tests for the addon migration search_path (QA 7Leguas 0922,
// "MIGRATION-SCHEMA"). The host (ops) materialises every addon model in
// `public`, while `addon_<key>` still carries empty twins of the same tables.
// With the default `search_path addon_<key>, public` a bare
// `CREATE UNIQUE INDEX … ON stock(…) WHERE deleted_at IS NULL` resolved to
// the twin (no deleted_at) and failed — inventory@017 — or silently indexed
// the empty table — purchases@008.
//
// The Postgres cases run when TEST_POSTGRES_DSN points to a scratch database
// (CI job "postgres"); they skip otherwise.

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMigrationSearchPath_DefaultsWithoutHostSchema(t *testing.T) {
	cases := []ApplyOptions{
		{},
		{PrimarySchema: "addon_inventory", ModelTables: []string{"stock"}},
		{PrimarySchema: "public"}, // no model tables to look for
	}
	for _, opts := range cases {
		// tx is never touched on these paths.
		got, err := migrationSearchPath(nil, "addon_inventory", opts)
		if err != nil {
			t.Fatalf("%+v: %v", opts, err)
		}
		if strings.Join(got, ",") != "addon_inventory,public" {
			t.Fatalf("%+v: search_path = %v, want addon_inventory,public", opts, got)
		}
	}
}

func TestQuoteIdents(t *testing.T) {
	if got := quoteIdents([]string{"public", `we"ird`}); got != `"public", "we""ird"` {
		t.Fatalf("quoteIdents = %s", got)
	}
}

// pgTestDB opens TEST_POSTGRES_DSN and returns a random suffix so every test
// uses its own addon key / table names and never collides with real objects.
func pgTestDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres migration test")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return db, hex.EncodeToString(b)
}

func mustExec(t *testing.T, db *gorm.DB, sql string) {
	t.Helper()
	if err := db.Exec(sql).Error; err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// indexSchema returns the schema of the table the named index was built on
// ("" when the index does not exist).
func indexSchema(t *testing.T, db *gorm.DB, index string) string {
	t.Helper()
	var schema string
	if err := db.Raw(`SELECT n.nspname FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_catalog.pg_class tc ON tc.oid = i.indrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = tc.relnamespace
		WHERE ic.relname = ?`, index).Scan(&schema).Error; err != nil {
		t.Fatalf("lookup index %s: %v", index, err)
	}
	return schema
}

func ledgerHas(t *testing.T, db *gorm.DB, key, version string) bool {
	t.Helper()
	var n int64
	if err := db.Table("public.metacore_addon_migrations").
		Where("addon_key = ? AND version = ?", key, version).Count(&n).Error; err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return n > 0
}

// twinFixture reproduces prod: the live `stock` table in public (with
// deleted_at and rows) and an empty twin in addon_<key> without deleted_at.
func twinFixture(t *testing.T, db *gorm.DB, sfx string) (key, schema, table string) {
	t.Helper()
	key = "inv" + sfx
	schema = SchemaName(key, uuid.Nil, IsolationShared)
	table = "stock_" + sfx
	mustExec(t, db, `CREATE SCHEMA "`+schema+`"`)
	mustExec(t, db, `CREATE TABLE public.`+table+` (id serial PRIMARY KEY, organization_id uuid, product_id uuid, warehouse_id uuid, deleted_at timestamptz)`)
	mustExec(t, db, `INSERT INTO public.`+table+` (organization_id, product_id, warehouse_id) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid())`)
	mustExec(t, db, `CREATE TABLE "`+schema+`".`+table+` (id serial PRIMARY KEY, organization_id uuid, product_id uuid, warehouse_id uuid)`)
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		db.Exec(`DROP TABLE IF EXISTS public.` + table + ` CASCADE`)
		db.Exec(`DROP TABLE IF EXISTS public.helper_` + sfx + ` CASCADE`)
		db.Exec(`DELETE FROM public.metacore_addon_migrations WHERE addon_key = ?`, key)
	})
	return key, schema, table
}

// The inventory@017 shape: an unqualified partial unique index over the live
// table. Before the fix it resolved to the empty twin and failed with
// "column deleted_at does not exist"; with the host schema declared it lands
// on public.
func TestApplyPostgres_UnqualifiedMigrationTargetsHostTable(t *testing.T) {
	db, sfx := pgTestDB(t)
	key, _, table := twinFixture(t, db, sfx)
	idx := "stock_cell_uq_" + sfx
	mig := File{Version: "017_stock_cell_unique", SQL: `CREATE UNIQUE INDEX IF NOT EXISTS ` + idx +
		` ON ` + table + ` (organization_id, product_id, warehouse_id) WHERE deleted_at IS NULL`}

	// Old behaviour (no host schema): the bare name hits the twin.
	err := Apply(db, key, uuid.Nil, IsolationShared, []File{mig})
	if err == nil || !strings.Contains(err.Error(), "deleted_at") {
		t.Fatalf("default search_path: want the twin's `deleted_at does not exist`, got %v", err)
	}
	if ledgerHas(t, db, key, mig.Version) {
		t.Fatalf("failed migration must not be recorded")
	}

	opts := ApplyOptions{PrimarySchema: "public", ModelTables: []string{table}}
	if err := ApplyWithOptions(db, key, uuid.Nil, IsolationShared, []File{mig}, opts); err != nil {
		t.Fatalf("ApplyWithOptions: %v", err)
	}
	if got := indexSchema(t, db, idx); got != "public" {
		t.Fatalf("index %s built on schema %q, want public", idx, got)
	}
	if !ledgerHas(t, db, key, mig.Version) {
		t.Fatalf("migration not recorded in the ledger")
	}
}

// Already-applied migrations are never re-run, whatever the options: the
// ledger decides, not the search_path.
func TestApplyPostgres_AppliedMigrationsAreNotReRun(t *testing.T) {
	db, sfx := pgTestDB(t)
	key, _, table := twinFixture(t, db, sfx)
	first := File{Version: "001_mark", SQL: `INSERT INTO ` + table + ` (organization_id) VALUES (NULL)`}
	if err := Apply(db, key, uuid.Nil, IsolationShared, []File{first}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	opts := ApplyOptions{PrimarySchema: "public", ModelTables: []string{table}}
	if err := ApplyWithOptions(db, key, uuid.Nil, IsolationShared, []File{first}, opts); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	var nullRows int64
	db.Raw(`SELECT count(*) FROM public.` + table + ` WHERE organization_id IS NULL`).Scan(&nullRows)
	if nullRows != 0 {
		t.Fatalf("applied migration re-ran against public (%d rows)", nullRows)
	}
}

// Migrations that already walk the schemas explicitly resolve the same way,
// tables that only exist in the addon schema still resolve (it stays second
// in the path), and to_regclass('<bare>') now sees the host table.
func TestApplyPostgres_QualifiedAndAddonOnlyTablesUnchanged(t *testing.T) {
	db, sfx := pgTestDB(t)
	key, schema, table := twinFixture(t, db, sfx)
	mustExec(t, db, `CREATE TABLE "`+schema+`".helper_`+sfx+` (id int)`)
	opts := ApplyOptions{PrimarySchema: "public", ModelTables: []string{table}}
	files := []File{
		{Version: "010_qualified", SQL: `CREATE INDEX IF NOT EXISTS twin_idx_` + sfx + ` ON "` + schema + `".` + table + ` (product_id)`},
		{Version: "011_addon_only", SQL: `INSERT INTO helper_` + sfx + ` (id) VALUES (1)`},
		{Version: "012_regclass", SQL: `DO $$ BEGIN
			IF to_regclass('` + table + `') IS DISTINCT FROM to_regclass('public.` + table + `') THEN
				RAISE EXCEPTION 'to_regclass resolved %', to_regclass('` + table + `');
			END IF; END $$`},
	}
	if err := ApplyWithOptions(db, key, uuid.Nil, IsolationShared, files, opts); err != nil {
		t.Fatalf("ApplyWithOptions: %v", err)
	}
	if got := indexSchema(t, db, "twin_idx_"+sfx); got != schema {
		t.Fatalf("qualified index landed on %q, want %q", got, schema)
	}
	var n int64
	db.Raw(`SELECT count(*) FROM "` + schema + `".helper_` + sfx).Scan(&n)
	if n != 1 {
		t.Fatalf("addon-only table did not resolve through the addon schema")
	}
}

// First install: the host has not materialised the models in public yet, so
// the default order is kept and the migration lands on the addon schema.
func TestApplyPostgres_HostTablesMissingKeepsAddonSchemaFirst(t *testing.T) {
	db, sfx := pgTestDB(t)
	key := "inv" + sfx
	schema := SchemaName(key, uuid.Nil, IsolationShared)
	table := "stock_" + sfx
	mustExec(t, db, `CREATE SCHEMA "`+schema+`"`)
	mustExec(t, db, `CREATE TABLE "`+schema+`".`+table+` (id int, deleted_at timestamptz)`)
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		db.Exec(`DELETE FROM public.metacore_addon_migrations WHERE addon_key = ?`, key)
	})
	idx := "fresh_idx_" + sfx
	opts := ApplyOptions{PrimarySchema: "public", ModelTables: []string{table}}
	mig := File{Version: "001_idx", SQL: `CREATE INDEX ` + idx + ` ON ` + table + ` (id) WHERE deleted_at IS NULL`}
	if err := ApplyWithOptions(db, key, uuid.Nil, IsolationShared, []File{mig}, opts); err != nil {
		t.Fatalf("ApplyWithOptions: %v", err)
	}
	if got := indexSchema(t, db, idx); got != schema {
		t.Fatalf("index landed on %q, want %q", got, schema)
	}
}
