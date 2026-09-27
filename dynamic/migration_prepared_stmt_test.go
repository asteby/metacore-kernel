package dynamic

// Regression test for the appliance install failure "apply customers@001_init.up:
// ERROR: cannot insert multiple commands into a prepared statement (SQLSTATE
// 42601)". ops opens its pool with gorm.Config{PrepareStmt: true}; GORM then
// PREPAREs every Exec, and Postgres rejects a prepared statement holding more
// than one command. A migration file is a script, so the runner must send it
// with the simple protocol whatever the host's GORM config.

import (
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestApply_MultiStatementScriptWithPreparedStmtPool(t *testing.T) {
	_, sfx := pgTestDB(t)
	db, err := gorm.Open(postgres.Open(os.Getenv("TEST_POSTGRES_DSN")), &gorm.Config{
		PrepareStmt: true,
		Logger:      logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	key := "ms" + sfx
	schema := SchemaName(key, uuid.Nil, IsolationShared)
	mustExec(t, db, `CREATE SCHEMA "`+schema+`"`)
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		db.Exec(`DELETE FROM public.metacore_addon_migrations WHERE addon_key = ?`, key)
	})

	// Two plain statements, a DO block whose body carries its own ';' and a
	// function in dollar quotes: splitting on ';' would break both.
	script := `
CREATE TABLE customers (id serial PRIMARY KEY, name text NOT NULL);
CREATE INDEX idx_customers_name ON customers (name);
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM customers) THEN
    INSERT INTO customers (name) VALUES ('uno; con punto y coma');
  END IF;
END
$$;
CREATE FUNCTION customers_count() RETURNS bigint LANGUAGE sql AS $fn$
  SELECT count(*) FROM customers;
$fn$;
`
	if err := Apply(db, key, uuid.Nil, IsolationShared, []File{{Version: "001_init.up", SQL: script}}); err != nil {
		t.Fatalf("apply multi-statement migration: %v", err)
	}
	var n int64
	if err := db.Raw(`SELECT count(*) FROM "` + schema + `".customers`).Scan(&n).Error; err != nil {
		t.Fatalf("count migrated table: %v", err)
	}
	var fn bool
	db.Raw(`SELECT to_regprocedure(?) IS NOT NULL`, schema+".customers_count()").Scan(&fn)
	if !fn {
		t.Fatal("dollar-quoted function was not created")
	}
	if n != 1 {
		t.Fatalf("customers rows = %d, want 1 (DO block did not run)", n)
	}
	if !ledgerHas(t, db, key, "001_init.up") {
		t.Fatalf("ledger row for %s@001_init.up missing", key)
	}

	// A failing script still rolls back as a whole and reports the error.
	bad := "CREATE TABLE half_done (id int);\nSELECT * FROM no_such_table;"
	if err := Apply(db, key, uuid.Nil, IsolationShared, []File{{Version: "002_bad.up", SQL: bad}}); err == nil {
		t.Fatal("apply of a broken migration returned nil")
	}
	var exists bool
	db.Raw(`SELECT to_regclass(?) IS NOT NULL`, schema+".half_done").Scan(&exists)
	if exists {
		t.Fatal("half_done survived a failed migration: script was not atomic")
	}
	if ledgerHas(t, db, key, "002_bad.up") {
		t.Fatal("failed migration recorded in the ledger")
	}
}
