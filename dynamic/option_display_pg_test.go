package dynamic

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// The option display on the real dialect: uuid foreign keys (CAST … AS TEXT
// grouping, uuid-typed IN lists and context values), numeric aggregates and
// the branch scope through warehouses.
func TestPostgres_OptionDisplayMetrics(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	schema := "optd_" + hex.EncodeToString(b)
	if err := db.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	table := func(n string) string { return schema + "." + n }
	base := `id uuid PRIMARY KEY, organization_id uuid, created_by_id uuid, created_at timestamptz, updated_at timestamptz, deleted_at timestamptz, `
	f := newOptdFixtureOn(t, db, []string{
		`CREATE TABLE ` + table("test_optd_products") + ` (` + base + `name text, sku text, size_code text, unit_price numeric(18,4), min_stock numeric(18,4), image text, product_type text)`,
		`CREATE TABLE ` + table("test_optd_stock") + ` (` + base + `product_id uuid, warehouse_id uuid, available numeric(18,4))`,
		`CREATE TABLE ` + table("test_optd_warehouses") + ` (` + base + `name text, branch_id uuid)`,
	}, table, []v3.OptionMetric{stockMetric()})

	opts := f.options(t, OptionsQuery{})
	if v := trailingByKey(opts[f.tire.String()].Display)["stock"].Value; v != float64(8) {
		t.Fatalf("stock = %v, want 8", v)
	}
	oil := trailingByKey(opts[f.oil.String()].Display)["stock"]
	if oil.Value != float64(0) || oil.Text != "Agotado" {
		t.Fatalf("oil = %+v", oil)
	}
	opts = f.options(t, OptionsQuery{Context: map[string]string{"branch_id": f.branch.String()}})
	if v := trailingByKey(opts[f.tire.String()].Display)["stock"]; v.Value != float64(3) || v.Tone != "warning" {
		t.Fatalf("branch stock = %+v, want 3 warning", v)
	}
	opts = f.options(t, OptionsQuery{Context: map[string]string{"warehouse_id": "nope"}})
	if _, ok := trailingByKey(opts[f.tire.String()].Display)["stock"]; ok {
		t.Fatal("invalid uuid context must omit the metric (no 22P02)")
	}
	opts = f.options(t, OptionsQuery{IDs: []string{f.tire.String()}})
	if v := trailingByKey(opts[f.tire.String()].Display)["stock"].Value; v != float64(8) {
		t.Fatalf("ids mode stock = %v", v)
	}
}
