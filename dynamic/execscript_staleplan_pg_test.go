package dynamic

// Regresión: una migración con varias sentencias debe aplicarse aunque el host
// abra GORM con PrepareStmt y instale database/staleplan (OPS 0.1.18). El
// envoltorio de staleplan no lo desenvolvía execScript y Postgres rechazaba el
// archivo con SQLSTATE 42601. Necesita TEST_POSTGRES_DSN (ver
// docs/CACHE-COHERENCE.md §5).

import (
	"os"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/database/staleplan"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openStalePlanDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		PrepareStmt: true,
		Logger:      logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := staleplan.Install(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func multiStmtFiles(tbl string) []File {
	return []File{
		{Version: "001_create.up", SQL: `CREATE TABLE ` + tbl + ` (id int primary key, note text);`},
		{Version: "002_two_statements.up", SQL: `
ALTER TABLE ` + tbl + ` ADD COLUMN IF NOT EXISTS source_kind text;
CREATE UNIQUE INDEX IF NOT EXISTS ` + tbl + `_uq ON ` + tbl + ` (source_kind) WHERE source_kind IS NOT NULL;
`},
		{Version: "003_do_block.up", SQL: `
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = '` + tbl + `' AND column_name = 'extra') THEN
    ALTER TABLE ` + tbl + ` ADD COLUMN extra int;
  END IF;
END $$;
UPDATE ` + tbl + ` SET extra = 1 WHERE extra IS NULL;
`},
	}
}

func pgHasColumn(t *testing.T, db *gorm.DB, tbl, col string) bool {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ?`, tbl, col).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestPostgres_ApplyMultiStatementMigrationWithStalePlan(t *testing.T) {
	db := openStalePlanDB(t)
	tbl := "es_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS ` + tbl) })
	key := "esfix" + tbl[3:]
	t.Cleanup(func() { db.Exec(`DELETE FROM public.metacore_addon_migrations WHERE addon_key = ?`, key) })

	if err := ApplyWithOptions(db, key, uuid.Nil, IsolationShared, multiStmtFiles(tbl), ApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !pgHasColumn(t, db, tbl, "source_kind") || !pgHasColumn(t, db, tbl, "extra") {
		t.Fatal("la migración multi-sentencia no se aplicó completa")
	}
}

func TestPostgres_DryRunMultiStatementMigrationWithStalePlan(t *testing.T) {
	db := openStalePlanDB(t)
	tbl := "es_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS ` + tbl) })
	key := "esdry" + tbl[3:]
	if err := db.AutoMigrate(&Migration{}); err != nil {
		t.Fatal(err)
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := DryRunMigrations(tx, key, uuid.Nil, IsolationShared, multiStmtFiles(tbl), ApplyOptions{}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if pgHasColumn(t, db, tbl, "source_kind") {
		t.Fatal("el dry-run dejó rastro tras el rollback")
	}
}
