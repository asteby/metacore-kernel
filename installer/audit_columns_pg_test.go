package installer

// Audit-column standard on upgrade (docs/audit-columns.md). Reproduces the
// customers@037 incident of 4-oct-2026: a migration creates
// `TRIGGER … UPDATE OF deleted_at ON <table>` while the host-materialized table
// (in `public`) was created WITHOUT deleted_at, and the upgrade died with
// "column deleted_at of relation … does not exist". The installer's schema step
// now adds the missing standard columns BEFORE the migrations.
//
// Runs when TEST_POSTGRES_DSN points to a scratch database; skips otherwise.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
)

func TestInstallUpgrade_AuditColumnsAddedBeforeMigrations(t *testing.T) {
	db, key, table, _, m := freshInstallFixture(t)
	t.Cleanup(func() { db.Exec(`DROP FUNCTION IF EXISTS public.` + table + `_aud() CASCADE`) })

	// A pre-standard table, as the host materialized it long ago: no
	// deleted_at, no *_by_id.
	if err := db.Exec(`CREATE TABLE public.` + table + ` (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), organization_id uuid NOT NULL,
		qty int, seq int, created_at timestamp NOT NULL DEFAULT NOW(), updated_at timestamp NOT NULL DEFAULT NOW())`).Error; err != nil {
		t.Fatal(err)
	}
	trigger := dynamic.File{Version: "037_trigger", SQL: `
CREATE OR REPLACE FUNCTION ` + table + `_aud() RETURNS trigger AS $$
BEGIN RETURN NEW; END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_aud ON ` + table + `;
CREATE TRIGGER trg_aud AFTER UPDATE OF deleted_at ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + table + `_aud();`}

	org := uuid.New()
	inst := freshInstaller(db) // MigrationSchema → public
	if _, _, err := inst.Install(org, &bundle.Bundle{Manifest: m}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Upgrade carrying the 037-shaped migration: the pre-flight dry run AND the
	// real apply must both find deleted_at.
	b2 := &bundle.Bundle{Manifest: m, Migrations: []dynamic.File{trigger}}
	b2.Manifest.Version = "1.1.0"
	if _, err := inst.Upgrade(t.Context(), org, b2); err != nil {
		t.Fatalf("Upgrade with a migration assuming deleted_at: %v", err)
	}
	for _, col := range []string{"deleted_at", "created_by_id", "updated_by_id", "deleted_by_id"} {
		var n int64
		db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=? AND column_name=?`, table, col).Scan(&n)
		if n != 1 {
			t.Errorf("public.%s.%s was not added by the schema step", table, col)
		}
	}
	// The kernel-added timestamp follows the table's own (timestamp) type.
	var typ string
	db.Raw(`SELECT data_type FROM information_schema.columns WHERE table_schema='public' AND table_name=? AND column_name='deleted_at'`, table).Scan(&typ)
	if typ != "timestamp without time zone" {
		t.Errorf("deleted_at type = %q, want timestamp without time zone (match created_at)", typ)
	}
	// Idempotent: running the schema step again changes nothing and does not fail.
	if err := dynamic.EnsureAuditColumns(db, "public", m.ModelDefinitions[0]); err != nil {
		t.Fatalf("EnsureAuditColumns is not idempotent: %v", err)
	}
	// The addon-schema twin created by CreateTable carries them too.
	var n int64
	db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema=? AND table_name=? AND column_name IN ('deleted_at','created_by_id','updated_by_id','deleted_by_id')`, "addon_"+key, table).Scan(&n)
	if n != 4 {
		t.Errorf("addon_%s.%s carries %d/4 audit columns", key, table, n)
	}
}
