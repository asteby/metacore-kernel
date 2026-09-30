package dynamic

import (
	"testing"

	"github.com/google/uuid"
)

func TestDeclaresReplayAfterModels(t *testing.T) {
	yes := []string{
		"-- metacore: replay-after-models\nCREATE TABLE x();",
		"-- addon-x 020\n--   metacore:replay-after-models\nSELECT 1;",
		"  -- METACORE: Replay-After-Models (idempotent)\nSELECT 1;",
	}
	no := []string{
		"CREATE TABLE x();",
		"-- this migration does not replay-after-models",
		"SELECT '-- metacore: replay-after-models';",
	}
	for _, s := range yes {
		if !declaresReplayAfterModels(s) {
			t.Errorf("want marker in %q", s)
		}
	}
	for _, s := range no {
		if declaresReplayAfterModels(s) {
			t.Errorf("no marker expected in %q", s)
		}
	}
}

// The inventory@019 shape: a trigger over a table the host materialises AFTER
// the installer ran the migration. Apply on a first install finds nothing to
// attach to and records the file; ReplayAfterModels attaches it once the table
// exists, and is a no-op-safe repeat.
func TestReplayAfterModelsPostgres_AttachesOnceHostTableExists(t *testing.T) {
	db, sfx := pgTestDB(t)
	key := "inv" + sfx
	schema := SchemaName(key, uuid.Nil, IsolationShared)
	table := "mov_" + sfx
	mustExec(t, db, `CREATE SCHEMA "`+schema+`"`)
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		db.Exec(`DROP TABLE IF EXISTS public.` + table + ` CASCADE`)
		db.Exec(`DROP FUNCTION IF EXISTS public.fn_` + sfx + `()`)
		db.Exec(`DELETE FROM public.metacore_addon_migrations WHERE addon_key = ?`, key)
	})
	mig := File{Version: "020_trigger.up", SQL: `-- metacore: replay-after-models
DO $m$
BEGIN
    IF to_regclass('` + table + `') IS NULL THEN RETURN; END IF;
    CREATE OR REPLACE FUNCTION public.fn_` + sfx + `() RETURNS trigger LANGUAGE plpgsql AS $f$
    BEGIN NEW.note := 'stamped'; RETURN NEW; END $f$;
    DROP TRIGGER IF EXISTS trg_` + sfx + ` ON ` + table + `;
    CREATE TRIGGER trg_` + sfx + ` BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION public.fn_` + sfx + `();
END $m$;`}
	opts := ApplyOptions{PrimarySchema: "public", ModelTables: []string{table}}

	// First install: the host has not created the table yet.
	if err := ApplyWithOptions(db, key, uuid.Nil, IsolationShared, []File{mig}, opts); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !ledgerHas(t, db, key, mig.Version) {
		t.Fatalf("migration must be recorded")
	}
	// Host materialises the model afterwards.
	mustExec(t, db, `CREATE TABLE public.`+table+` (id serial PRIMARY KEY, note text)`)
	mustExec(t, db, `INSERT INTO public.`+table+` (note) VALUES ('raw')`)
	var note string
	db.Raw(`SELECT note FROM public.` + table).Scan(&note)
	if note != "raw" {
		t.Fatalf("no trigger yet, note = %q", note)
	}

	for i := 0; i < 2; i++ { // idempotent on repeat
		if err := ReplayAfterModels(db, key, uuid.Nil, IsolationShared, []File{mig}, opts); err != nil {
			t.Fatalf("replay #%d: %v", i, err)
		}
	}
	mustExec(t, db, `INSERT INTO public.`+table+` (note) VALUES ('after')`)
	db.Raw(`SELECT note FROM public.` + table + ` WHERE id = 2`).Scan(&note)
	if note != "stamped" {
		t.Fatalf("trigger not attached by replay, note = %q", note)
	}
}

func TestReplayAfterModels_SkipsUnmarked(t *testing.T) {
	// db is never touched: an unmarked file must not open a transaction.
	if err := ReplayAfterModels(nil, "x", uuid.Nil, IsolationShared,
		[]File{{Version: "001", SQL: "CREATE TABLE boom();"}}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
}
