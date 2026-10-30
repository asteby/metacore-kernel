package dynamic

// Candado de migraciones por addon (lockAddonDDL). En ops dos rutas de arranque
// re-registraban `inventory` a la vez y ambas reaplicaban la misma migración de
// replay (020_kardex_trigger_replay): dos CREATE OR REPLACE FUNCTION
// simultáneos de la misma función hacen que Postgres aborte uno con
// "tuple concurrently updated" (SQLSTATE XX000). La parte Postgres necesita
// TEST_POSTGRES_DSN (se salta si no está).

import (
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestLockAddonDDL_NoopOutsidePostgres(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := lockAddonDDL(nil, "inventory"); err != nil {
		t.Fatalf("nil db: want no-op, got %v", err)
	}
	tx := db.Begin()
	if err := lockAddonDDL(tx, "inventory"); err != nil {
		tx.Rollback()
		t.Fatalf("sqlite: want no-op, got %v", err)
	}
	// La transacción sigue sana: el no-op no envió nada que la aborte.
	if err := tx.Exec(`CREATE TABLE t_lock (id INTEGER)`).Error; err != nil {
		tx.Rollback()
		t.Fatalf("tx unusable after no-op lock: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// replayRaceFixture prepares a function in public and a replay migration that
// redefines it, holding the catalog row a moment so concurrent runners overlap.
func replayRaceFixture(t *testing.T) (*gorm.DB, string, File) {
	t.Helper()
	db, sfx := pgTestDB(t)
	key := "invlock" + sfx
	schema := SchemaName(key, uuid.Nil, IsolationShared)
	fn := "public.kardex_" + sfx
	mustExec(t, db, `CREATE SCHEMA "`+schema+`"`)
	mustExec(t, db, `CREATE FUNCTION `+fn+`() RETURNS int LANGUAGE sql AS 'SELECT 0'`)
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		db.Exec(`DROP FUNCTION IF EXISTS ` + fn + `()`)
	})
	mig := File{Version: "020_kardex_trigger_replay.up", SQL: `-- metacore: replay-after-models
CREATE OR REPLACE FUNCTION ` + fn + `() RETURNS int LANGUAGE sql AS 'SELECT 20';
SELECT pg_sleep(0.05);`}
	return db, key, mig
}

const raceRunners = 8

// runConcurrently starts raceRunners goroutines at once and returns their errors.
func runConcurrently(run func() error) []error {
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		errs  []error
	)
	for i := 0; i < raceRunners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := run(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// Sin candado: la misma secuencia que hacía ReplayAfterModels antes del fix
// (BEGIN, script, COMMIT, en conexiones distintas) reproduce el XX000.
func TestReplayAfterModelsPostgres_RaceWithoutLockReproducesXX000(t *testing.T) {
	db, _, mig := replayRaceFixture(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(raceRunners)
	var saw bool
	for round := 0; round < 5 && !saw; round++ {
		errs := runConcurrently(func() error {
			tx := db.Begin()
			if err := execScript(tx, mig.SQL); err != nil {
				tx.Rollback()
				return err
			}
			return tx.Commit().Error
		})
		for _, err := range errs {
			if !strings.Contains(err.Error(), "tuple concurrently updated") {
				t.Fatalf("unexpected error: %v", err)
			}
			saw = true
		}
	}
	if !saw {
		t.Fatal("expected SQLSTATE XX000 'tuple concurrently updated' without the lock")
	}
}

// Con candado: N re-registros simultáneos del mismo addon se serializan.
func TestReplayAfterModelsPostgres_ConcurrentReplaysAreSerialised(t *testing.T) {
	db, key, mig := replayRaceFixture(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(raceRunners)
	for round := 0; round < 5; round++ {
		errs := runConcurrently(func() error {
			return ReplayAfterModels(db, key, uuid.Nil, IsolationShared, []File{mig}, ApplyOptions{})
		})
		for _, err := range errs {
			t.Errorf("round %d: concurrent replay failed: %v", round, err)
		}
	}
}

// Con candado: N instalaciones simultáneas de la misma migración pendiente la
// ejecutan una sola vez (el segundo runner relee el ledger tras esperar).
func TestApplyPostgres_ConcurrentAppliesRunPendingFileOnce(t *testing.T) {
	db, sfx := pgTestDB(t)
	key := "invapply" + sfx
	schema := SchemaName(key, uuid.Nil, IsolationShared)
	counter := "public.runs_" + sfx
	mustExec(t, db, `CREATE SCHEMA "`+schema+`"`)
	mustExec(t, db, `CREATE TABLE `+counter+` (n int)`)
	if err := db.AutoMigrate(&Migration{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		db.Exec(`DROP TABLE IF EXISTS ` + counter)
		db.Exec(`DELETE FROM public.metacore_addon_migrations WHERE addon_key = ?`, key)
	})
	mustExec(t, db, `CREATE FUNCTION public.fn_`+sfx+`() RETURNS int LANGUAGE sql AS 'SELECT 0'`)
	t.Cleanup(func() { db.Exec(`DROP FUNCTION IF EXISTS public.fn_` + sfx + `()`) })
	mig := File{Version: "001_once.up", SQL: `CREATE OR REPLACE FUNCTION public.fn_` + sfx + `() RETURNS int LANGUAGE sql AS 'SELECT 1';
INSERT INTO ` + counter + ` VALUES (1); SELECT pg_sleep(0.05);`}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(raceRunners)
	errs := runConcurrently(func() error {
		return Apply(db, key, uuid.Nil, IsolationShared, []File{mig})
	})
	for _, err := range errs {
		t.Errorf("concurrent apply failed: %v", err)
	}
	var n int64
	if err := db.Raw(`SELECT count(*) FROM ` + counter).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pending migration ran %d times, want 1", n)
	}
}
