package dynamic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/manifest"
)

// Real Postgres (TEST_POSTGRES_DSN; skipped without it): build, idempotency,
// dry-run, INVALID rebuild, busy lock, lock_timeout, transaction refusal,
// schema targeting and the core-table guard.
func TestPostgres_EnsureDeclaredIndexes(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	open := func() *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1) // the dedicated connection is the pool's only one: settings must be reset on it
	other := open()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	addon := "addon_dix_" + hex.EncodeToString(b)
	core := "core_dix_" + hex.EncodeToString(b)
	for _, s := range []string{addon, core} {
		if err := other.Exec(`CREATE SCHEMA ` + s).Error; err != nil {
			t.Fatal(err)
		}
		s := s
		t.Cleanup(func() { other.Exec(`DROP SCHEMA ` + s + ` CASCADE`) })
		if err := other.Exec(`CREATE TABLE ` + s + `.crm_leads (id uuid PRIMARY KEY, organization_id uuid, customer_id uuid, status text)`).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := other.Exec(`INSERT INTO ` + addon + `.crm_leads SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', NULL, 'open' FROM generate_series(1, 50)`).Error; err != nil {
		t.Fatal(err)
	}
	defs := []manifest.ModelDefinition{{
		ModelKey: "Lead", TableName: "crm_leads",
		Indices: []manifest.IndexDef{{Name: "crm_leads_org_customer_idx", Columns: []string{"organization_id", "customer_id"}}},
	}, {
		ModelKey: "Conversation", TableName: "conversations",
		Indices: []manifest.IndexDef{{Name: "conversations_org_customer_idx", Columns: []string{"organization_id", "customer_id"}}},
	}}
	ctx := context.Background()
	opts := DeclaredIndexOptions{Schema: addon, Logger: quietLog(), LockTimeout: 300 * time.Millisecond}
	state := func(schema string) (exists, valid bool) {
		info, err := pgDeclaredIndexDB{db: other}.index(ctx, schema, "crm_leads_org_customer_idx")
		if err != nil {
			t.Fatal(err)
		}
		return info.exists, info.valid
	}

	// Dry-run: plan only.
	rep := EnsureDeclaredIndexes(ctx, db, defs, DeclaredIndexOptions{Schema: addon, DryRun: true, Logger: quietLog()})
	if r := rep.Results[0]; r.State != DeclaredIndexPlanned || len(r.Statements) != 1 {
		t.Fatalf("dry-run: %+v", r)
	}
	if r := rep.Results[1]; r.State != DeclaredIndexSkipped || r.Reason != DeclaredIndexReasonTableMissing {
		t.Fatalf("missing table: %+v", r)
	}
	if e, _ := state(addon); e {
		t.Fatal("dry-run built the index")
	}

	// Inside a transaction: refused, nothing built, never a plain CREATE INDEX.
	_ = db.Transaction(func(tx *gorm.DB) error {
		rep := EnsureDeclaredIndexes(ctx, tx, defs, opts)
		if !errors.Is(rep.Err, ErrDeclaredIndexInTx) || rep.Results[0].Reason != DeclaredIndexReasonInTransaction {
			t.Errorf("in tx: %+v", rep)
		}
		return nil
	})
	if e, _ := state(addon); e {
		t.Fatal("built inside a transaction")
	}

	// Busy: another session holds this index's lock.
	key := declaredIndexLockKey(addon, "crm_leads_org_customer_idx")
	if err := other.Connection(func(c *gorm.DB) error {
		var got bool
		c.Raw(`SELECT pg_try_advisory_lock(hashtextextended(?, 0))`, key).Scan(&got)
		if !got {
			t.Fatal("could not take the test lock")
		}
		defer c.Exec(`SELECT pg_advisory_unlock(hashtextextended(?, 0))`, key)
		if r := EnsureDeclaredIndexes(ctx, db, defs, opts).Results[0]; r.State != DeclaredIndexBusy {
			t.Errorf("busy: %+v", r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// lock_timeout: a long transaction holding a conflicting lock makes the
	// build fail fast (and leave no INVALID index), never wait for it.
	tx := other.Begin()
	if err := tx.Exec(`LOCK TABLE ` + addon + `.crm_leads IN SHARE ROW EXCLUSIVE MODE`).Error; err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := EnsureDeclaredIndexes(ctx, db, defs, opts).Results[0]
	tx.Rollback()
	if r.State != DeclaredIndexFailed || !strings.Contains(r.Error, "lock timeout") || time.Since(start) > 10*time.Second {
		t.Fatalf("lock timeout: %+v", r)
	}
	if e, _ := state(addon); e {
		t.Fatal("INVALID leftover after a lock timeout")
	}

	// Build: only in the addon schema; the same-named table elsewhere untouched.
	r = EnsureDeclaredIndexes(ctx, db, defs, opts).Results[0]
	if r.State != DeclaredIndexBuilt {
		t.Fatalf("build: %+v", r)
	}
	for _, s := range r.Statements {
		if !strings.Contains(s, "CONCURRENTLY") {
			t.Fatalf("non-concurrent statement: %s", s)
		}
	}
	if e, v := state(addon); !e || !v {
		t.Fatalf("after build exists=%v valid=%v", e, v)
	}
	if e, _ := state(core); e {
		t.Fatal("touched the same-named table in another schema")
	}
	if r := EnsureDeclaredIndexes(ctx, db, defs, opts).Results[0]; r.State != DeclaredIndexExists {
		t.Fatalf("rerun: %+v", r)
	}
	// Session settings were reset on the (only) pooled connection.
	var lt string
	db.Raw(`SHOW lock_timeout`).Scan(&lt)
	if lt != "0" {
		t.Fatalf("lock_timeout leaked onto the pool: %q", lt)
	}

	// INVALID: a failed CONCURRENTLY unique build leaves an INVALID index
	// under the declared name; the builder drops it and rebuilds.
	if err := other.Exec(`DROP INDEX ` + addon + `.crm_leads_org_customer_idx`).Error; err != nil {
		t.Fatal(err)
	}
	_ = other.Exec(`CREATE UNIQUE INDEX CONCURRENTLY crm_leads_org_customer_idx ON ` + addon + `.crm_leads (organization_id, status)`).Error
	if e, v := state(addon); !e || v {
		t.Fatalf("setup: want an INVALID index, exists=%v valid=%v", e, v)
	}
	if r := EnsureDeclaredIndexes(ctx, db, defs, opts).Results[0]; r.State != DeclaredIndexRebuilt {
		t.Fatalf("rebuild: %+v", r)
	}
	if e, v := state(addon); !e || !v {
		t.Fatalf("after rebuild exists=%v valid=%v", e, v)
	}

	// Core-table guard and allowlist: nothing is touched.
	rep = EnsureDeclaredIndexes(ctx, db, defs, DeclaredIndexOptions{Schema: core, CoreTables: []string{"crm_leads"}, Logger: quietLog()})
	if rep.Results[0].Reason != DeclaredIndexReasonCoreTable {
		t.Fatalf("core table: %+v", rep.Results[0])
	}
	rep = EnsureDeclaredIndexes(ctx, db, defs, DeclaredIndexOptions{Schema: core, OnlyIndexes: []string{"other_idx"}, Logger: quietLog()})
	if rep.Results[0].Reason != DeclaredIndexReasonNotInOnly {
		t.Fatalf("only: %+v", rep.Results[0])
	}
	if e, _ := state(core); e {
		t.Fatal("core/allowlist run built an index")
	}
}
