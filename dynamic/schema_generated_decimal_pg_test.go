package dynamic

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/manifest"
)

// QA 0927: a peso mirror of a cents column, `total_amount_cents / 100.0`, lost
// its cents in Postgres (990997 → 9909) because the literal was rendered as
// `100` and the division became integer.
func TestPostgres_GeneratedDecimalLiteralKeepsCents(t *testing.T) {
	c := manifest.ColumnDef{Name: "total_amount", Type: "numeric", Generated: "total_amount_cents / 100.0"}
	got, err := addColumnDDL("addon_ws", "work_orders", c)
	if err != nil {
		t.Fatalf("addColumnDDL: %v", err)
	}
	want := `ALTER TABLE "addon_ws"."work_orders" ADD COLUMN IF NOT EXISTS "total_amount" numeric(18,4) GENERATED ALWAYS AS (("total_amount_cents" / 100.0)) STORED`
	if got != want {
		t.Fatalf("addColumnDDL mismatch:\nwant: %s\ngot:  %s", want, got)
	}

	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	schema := "g_" + hex.EncodeToString(b)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	if err := db.Exec(`CREATE TABLE ` + schema + `.work_orders (id int PRIMARY KEY, total_amount_cents bigint NOT NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	ddl, err := addColumnDDL(schema, "work_orders", c)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(ddl).Error; err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	if err := db.Exec(`INSERT INTO ` + schema + `.work_orders VALUES (1, 990997)`).Error; err != nil {
		t.Fatal(err)
	}
	var total string
	if err := db.Raw(`SELECT total_amount::text FROM ` + schema + `.work_orders WHERE id = 1`).Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	if total != "9909.9700" {
		t.Fatalf("total_amount = %s, want 9909.9700", total)
	}
}
