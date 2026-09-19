package dynamic

import (
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
