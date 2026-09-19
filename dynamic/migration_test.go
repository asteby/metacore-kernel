package dynamic

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestApply_SkipsLedgerRowsWithoutChainingWhereClauses(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// The ledger lives in the "public" schema; emulate it with an attached DB
	// on a single connection (ATTACH is per-connection).
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec("ATTACH DATABASE ':memory:' AS public").Error; err != nil {
		t.Fatalf("attach public: %v", err)
	}
	if err := db.Exec(`CREATE TABLE public.metacore_addon_migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		addon_key TEXT NOT NULL,
		version TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at DATETIME,
		UNIQUE (addon_key, version))`).Error; err != nil {
		t.Fatalf("create public ledger: %v", err)
	}

	const sql = "THIS SQL MUST NOT EXECUTE"
	files := []File{
		{Version: "001_init.up", SQL: sql},
		{Version: "002_orders_and_drift.up", SQL: sql},
		{Version: "003_sales_order_customer_nullable.up", SQL: sql},
	}
	for _, f := range files {
		row := Migration{AddonKey: "customers", Version: f.Version, Checksum: Checksum(f.SQL)}
		if err := db.Table("public.metacore_addon_migrations").Create(&row).Error; err != nil {
			t.Fatalf("seed %s: %v", f.Version, err)
		}
	}
	if err := Apply(db, "customers", uuid.New(), IsolationShared, files); err != nil {
		t.Fatalf("Apply should skip all recorded migrations, got: %v", err)
	}
}

func newLedgerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec("ATTACH DATABASE ':memory:' AS public").Error; err != nil {
		t.Fatalf("attach public: %v", err)
	}
	if err := db.Exec(`CREATE TABLE public.metacore_addon_migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT, addon_key TEXT NOT NULL, version TEXT NOT NULL,
		checksum TEXT NOT NULL, applied_at DATETIME, UNIQUE (addon_key, version))`).Error; err != nil {
		t.Fatalf("create public ledger: %v", err)
	}
	return db
}

func ledgerChecksum(t *testing.T, db *gorm.DB, version string) string {
	t.Helper()
	var m Migration
	if err := db.Table("public.metacore_addon_migrations").Where("addon_key = ? AND version = ?", "customers", version).First(&m).Error; err != nil {
		t.Fatalf("read ledger %s: %v", version, err)
	}
	return m.Checksum
}

func TestApply_TolerantOfDeclaredInPlaceEdit(t *testing.T) {
	db := newLedgerDB(t)
	// Recorded with an older revision of the file; it must NOT be re-executed.
	db.Table("public.metacore_addon_migrations").Create(&Migration{AddonKey: "customers", Version: "004_x.up", Checksum: "old"})
	sql := "-- EDITED IN PLACE (exception to the immutable-migrations rule)\nTHIS SQL MUST NOT EXECUTE"
	if err := Apply(db, "customers", uuid.New(), IsolationShared, []File{{Version: "004_x.up", SQL: sql}}); err != nil {
		t.Fatalf("declared in-place edit should be tolerated, got: %v", err)
	}
	if got, want := ledgerChecksum(t, db, "004_x.up"), Checksum(sql); got != want {
		t.Fatalf("ledger checksum not pinned to the new file: got %s want %s", got, want)
	}
}

func TestApply_RefusesUndeclaredMutation(t *testing.T) {
	db := newLedgerDB(t)
	db.Table("public.metacore_addon_migrations").Create(&Migration{AddonKey: "customers", Version: "004_x.up", Checksum: "old"})
	err := Apply(db, "customers", uuid.New(), IsolationShared, []File{{Version: "004_x.up", SQL: "SELECT 1"}})
	if err == nil || !strings.Contains(err.Error(), "refusing to re-apply mutated SQL") {
		t.Fatalf("undeclared mutation must still be refused, got: %v", err)
	}
	if got := ledgerChecksum(t, db, "004_x.up"); got != "old" {
		t.Fatalf("ledger must be untouched on refusal, got %s", got)
	}
}
