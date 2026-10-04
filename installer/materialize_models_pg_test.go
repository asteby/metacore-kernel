package installer

// Fresh-tenant install (QA benchmark 29-sep, QA-W1-01/02, kernel#446). The host
// (ops) serves every addon model from `public` and creates those tables AFTER
// the installer ran the migrations, so on an empty database a migration that
// assumes `public.<table>` existed aborted (pos@011 42P01) or attached to the
// empty addon_<key> twin (inventory@019 kardex trigger). MaterializeModels
// makes the host create them first.
//
// Runs when TEST_POSTGRES_DSN points to a scratch database; skips otherwise.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
)

func freshInstallFixture(t *testing.T) (db *gorm.DB, key, table string, migs []dynamic.File, m manifest.Manifest) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres install test")
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	sfx := hex.EncodeToString(b)
	key, table = "kx"+sfx, "kx_moves_"+sfx
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS addon_` + key + ` CASCADE`)
		db.Exec(`DROP TABLE IF EXISTS public.` + table + ` CASCADE`)
		db.Exec(`DROP FUNCTION IF EXISTS public.` + table + `_fn() CASCADE`)
		db.Exec(`DELETE FROM public.metacore_addon_migrations WHERE addon_key = ?`, key)
		db.Exec(`DELETE FROM public.metacore_installations WHERE addon_key = ?`, key)
	})
	migs = []dynamic.File{
		// pos@011 shape: alters a public table the host must already have.
		{Version: "001_alter", SQL: `ALTER TABLE public.` + table + ` ADD COLUMN IF NOT EXISTS extra_id uuid;`},
		// inventory@019 shape: a trigger on the BARE table name (unmarked, so
		// replay-after-models would not cover it) filling seq/balance_after.
		{Version: "002_trigger", SQL: `
CREATE OR REPLACE FUNCTION ` + table + `_fn() RETURNS trigger AS $$
BEGIN
  NEW.seq := (SELECT COALESCE(MAX(seq), 0) + 1 FROM ` + table + `);
  RETURN NEW;
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_fill ON ` + table + `;
CREATE TRIGGER trg_fill BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + table + `_fn();`},
	}
	m = manifest.Manifest{Key: key, Name: key, Description: "x", Category: "utility", Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{{ModelKey: "Move", TableName: table,
			Columns: []manifest.ColumnDef{{Name: "qty", Type: "int"}, {Name: "seq", Type: "int"}}}}}
	return
}

func freshInstaller(db *gorm.DB) *Installer {
	inst := &Installer{DB: db, KernelVersion: "2.0.0", AllowUnsigned: true, Lifecycles: lifecycle.NewRegistry(), Broadcaster: NoopBroadcaster{}}
	return inst.WithMigrationSchema(func(string) string { return "public" })
}

// hostMaterializer stands in for ops' CreateDynamicTable: the host-shaped table
// in `public`, idempotent.
func hostMaterializer(db *gorm.DB, m manifest.Manifest) error {
	for _, def := range m.ModelDefinitions {
		if err := db.Exec(`CREATE TABLE IF NOT EXISTS public.` + def.TableName + ` (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), organization_id uuid NOT NULL,
			qty int, seq int)`).Error; err != nil {
			return err
		}
	}
	return nil
}

func TestInstall_FreshTenant_WithoutMaterializerBreaks(t *testing.T) {
	db, key, table, migs, m := freshInstallFixture(t)
	_ = table
	b := &bundle.Bundle{Manifest: m, Migrations: migs}
	if _, _, err := freshInstaller(db).Install(uuid.New(), b); err == nil {
		t.Fatal("control: without MaterializeModels a fresh tenant must fail (public.<table> missing); the fixture no longer reproduces kernel#446")
	}
	_ = key
}

func TestInstall_FreshTenant_MaterializeModelsFirst(t *testing.T) {
	db, key, table, migs, m := freshInstallFixture(t)
	b := &bundle.Bundle{Manifest: m, Migrations: migs}
	org := uuid.New()
	inst := freshInstaller(db).WithModelMaterializer(hostMaterializer)
	if _, _, err := inst.Install(org, b); err != nil {
		t.Fatalf("Install on a fresh tenant: %v", err)
	}
	// The trigger must live on public.<table> (not the addon_<key> twin).
	for i := 0; i < 3; i++ {
		if err := db.Exec(`INSERT INTO public.`+table+` (organization_id, qty) VALUES (?, 1)`, org).Error; err != nil {
			t.Fatal(err)
		}
	}
	var nulls int64
	db.Raw(`SELECT count(*) FROM public.` + table + ` WHERE seq IS NULL`).Scan(&nulls)
	if nulls != 0 {
		t.Fatalf("%d rows with seq NULL: the trigger did not attach to public.%s", nulls, table)
	}
	var col int64
	db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=? AND column_name='extra_id'`, table).Scan(&col)
	if col != 1 {
		t.Fatal("migration 001 (ALTER on public table) was not applied")
	}
	// Upgrade on the now existing tenant: materializer is a no-op, a new
	// migration lands on the live table.
	b2 := &bundle.Bundle{Manifest: m, Migrations: append(append([]dynamic.File{}, migs...),
		dynamic.File{Version: "003_more", SQL: `ALTER TABLE public.` + table + ` ADD COLUMN IF NOT EXISTS more_id uuid;`})}
	b2.Manifest.Version = "1.1.0"
	if _, err := inst.Upgrade(t.Context(), org, b2); err != nil {
		t.Fatalf("Upgrade on an existing tenant: %v", err)
	}
	db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=? AND column_name='more_id'`, table).Scan(&col)
	if col != 1 {
		t.Fatal("upgrade migration 003 not applied to the live table")
	}
	_ = key
}

// Upgrade pre-flight on real Postgres: a bundle whose 2nd new migration fails
// must leave the installed version, the ledger and the live schema untouched
// (the 1st new migration, which would have succeeded, is rehearsed and rolled
// back), and report a typed *UpgradeError{Phase: migrations, Preflight: true}.
func TestUpgrade_PreflightFailingMigration_LeavesInstallationIntact(t *testing.T) {
	db, key, table, migs, m := freshInstallFixture(t)
	org := uuid.New()
	inst := freshInstaller(db).WithModelMaterializer(hostMaterializer)
	if _, _, err := inst.Install(org, &bundle.Bundle{Manifest: m, Migrations: migs}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	b2 := &bundle.Bundle{Manifest: m, Migrations: append(append([]dynamic.File{}, migs...),
		dynamic.File{Version: "003_ok", SQL: `ALTER TABLE public.` + table + ` ADD COLUMN IF NOT EXISTS more_id uuid;`},
		dynamic.File{Version: "004_boom", SQL: `ALTER TABLE public.` + table + ` ADD COLUMN bad nosuchtype;`})}
	b2.Manifest.Version = "1.1.0"
	_, err := inst.Upgrade(t.Context(), org, b2)
	var ue *UpgradeError
	if !errors.As(err, &ue) || ue.Phase != UpgradePhaseMigrations || !ue.Preflight || ue.Addon != key || ue.Version != "1.1.0" {
		t.Fatalf("want preflight migrations UpgradeError, got %#v / %v", ue, err)
	}
	var ver string
	db.Raw(`SELECT version FROM public.metacore_installations WHERE organization_id = ? AND addon_key = ?`, org, key).Scan(&ver)
	if ver != "1.0.0" {
		t.Fatalf("installed version = %q, want 1.0.0", ver)
	}
	var n int64
	db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=? AND column_name='more_id'`, table).Scan(&n)
	if n != 0 {
		t.Fatal("003_ok leaked out of the dry-run transaction")
	}
	db.Raw(`SELECT count(*) FROM public.metacore_addon_migrations WHERE addon_key = ? AND version IN ('003_ok','004_boom')`, key).Scan(&n)
	if n != 0 {
		t.Fatal("dry run wrote the migration ledger")
	}
}
