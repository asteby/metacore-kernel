package installer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
)

// An upgrade that brings a `unique` rule over a table that already holds
// duplicates must NOT fail: the rule stays enforced by the application check
// and no index is built. Once the duplicates are gone, the next upgrade
// materializes the partial UNIQUE index.
func TestUpgrade_UniqueRuleWithDuplicatesDoesNotFailThenMaterializes(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	schema := "q_" + hex.EncodeToString(b)
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	db, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Installation{}); err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE metacore_addon_migrations (id serial PRIMARY KEY, addon_key text NOT NULL, version text NOT NULL, checksum text NOT NULL, applied_at timestamptz)`,
		`CREATE TABLE cash_registers (id uuid PRIMARY KEY, organization_id uuid, deleted_at timestamptz, branch_id uuid, code text, is_active boolean)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	org, branch := uuid.New(), uuid.New()
	dup := uuid.New()
	db.Exec(`INSERT INTO cash_registers VALUES (?, ?, NULL, ?, 'CJ1', true), (?, ?, NULL, ?, 'CJ1', true)`, uuid.New(), org, branch, dup, org, branch)
	seedInstallation(t, db, org, "pos", "1.0.0", nil)

	rule := manifest.CrossRuleDef{Kind: "unique", ErrorKey: "pos.cash_register.code_taken", Columns: []string{"branch_id", "code"}, Where: map[string]any{"is_active": true}}
	bundleAt := func(v string) {
		nb := makeBundle("pos", v, nil)
		nb.Manifest.ModelDefinitions = []manifest.ModelDefinition{{ModelKey: "CashRegister", TableName: "cash_registers",
			Columns: []manifest.ColumnDef{{Name: "branch_id", Type: "uuid"}, {Name: "code", Type: "string"}, {Name: "is_active", Type: "bool"}},
			Rules:   []manifest.CrossRuleDef{rule}}}
		inst := &Installer{DB: db, KernelVersion: "2.0.0", AllowUnsigned: true, Lifecycles: lifecycle.NewRegistry(), schemaApplier: &recordingApplier{}, Broadcaster: NoopBroadcaster{}}
		inst.WithMigrationSchema(func(string) string { return schema })
		if _, err := inst.Upgrade(context.Background(), org, nb); err != nil {
			t.Fatalf("Upgrade %s must not fail: %v", v, err)
		}
	}
	index := dynamic.UniqueIndexName("cash_registers", rule)
	indexExists := func() bool {
		var n int64
		db.Raw(`SELECT count(*) FROM pg_index WHERE indexrelid = to_regclass(?) AND indisvalid`, fmt.Sprintf("%q.%q", schema, index)).Scan(&n)
		return n == 1
	}

	bundleAt("1.1.0")
	if indexExists() {
		t.Fatal("no index while duplicates exist")
	}
	db.Exec(`UPDATE cash_registers SET is_active = false WHERE id = ?`, dup)
	bundleAt("1.2.0")
	if !indexExists() {
		t.Fatal("index must be materialized once the table is clean")
	}
}
