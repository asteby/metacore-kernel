package dynamic

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/manifest"
)

// Production 0.71.2 customers upgrade: the legacy twin addon_customers.invoices
// predates soft delete (no deleted_at). The model became soft-delete, so
// CreateTable asked for the (organization_id, deleted_at) index on a table that
// lacked the column and failed with 42703 — before any migration ran. A
// pre-existing table must be brought up to the audit standard BEFORE its
// indexes are built.
func TestPostgres_CreateTableSoftDeleteOnPreexistingTableWithoutDeletedAt(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	key := "sd" + hex.EncodeToString(b)
	schema := SchemaName(key, uuid.Nil, IsolationShared)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA "` + schema + `" CASCADE`) })
	// The table as an older release left it: org column, no deleted_at.
	if err := db.Exec(`CREATE TABLE "` + schema + `".invoices (id uuid PRIMARY KEY, organization_id uuid, created_at timestamptz DEFAULT now())`).Error; err != nil {
		t.Fatal(err)
	}
	def := manifest.ModelDefinition{TableName: "invoices", OrgScoped: true, SoftDelete: true}
	if err := CreateTable(db, key, uuid.Nil, IsolationShared, def); err != nil {
		t.Fatalf("CreateTable on a pre-existing table without deleted_at: %v", err)
	}
	var n int
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'invoices' AND column_name IN ('deleted_at','deleted_by_id','updated_at','created_by_id')`, schema).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("audit columns present = %d, want 4 (deleted_at, deleted_by_id, updated_at, created_by_id)", n)
	}
}
