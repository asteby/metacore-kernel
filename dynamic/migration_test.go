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
	if err := db.AutoMigrate(&Migration{}); err != nil {
		t.Fatalf("automigrate ledger: %v", err)
	}
	rows := []Migration{
		{AddonKey: "customers", Version: "001_init.up", Checksum: "a"},
		{AddonKey: "customers", Version: "002_orders_and_drift.up", Checksum: "b"},
		{AddonKey: "customers", Version: "003_sales_order_customer_nullable.up", Checksum: "c"},
	}
	for _, row := range rows {
		if err := db.Table("metacore_addon_migrations").Create(&row).Error; err != nil {
			t.Fatalf("seed %s: %v", row.Version, err)
		}
	}

	files := []File{
		{Version: "001_init.up", SQL: "THIS SQL MUST NOT EXECUTE"},
		{Version: "002_orders_and_drift.up", SQL: "THIS SQL MUST NOT EXECUTE"},
		{Version: "003_sales_order_customer_nullable.up", SQL: "THIS SQL MUST NOT EXECUTE"},
	}
	if err := Apply(db, "customers", uuid.New(), IsolationShared, files); err != nil {
		t.Fatalf("Apply should skip all recorded migrations, got: %v", err)
	}
}
