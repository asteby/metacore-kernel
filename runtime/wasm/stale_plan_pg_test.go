package wasm

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/database/staleplan"
)

// The production incident: an addon migration changes the result type of
// statements the pool had already prepared, and every later guest write fails
// (SQLSTATE 0A000) until the process restarts. With the stale-plan layer the
// data_mutate / data_query / db_query host calls heal on their own and a write
// is applied exactly once. Needs TEST_POSTGRES_DSN (skips otherwise).
func TestWasmDataHostHealsStalePlanAfterMigration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	// PrepareStmt is how OPS opens its pool: the configuration that made the
	// failure permanent.
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{PrepareStmt: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sdb, _ := db.DB()
	sdb.SetMaxOpenConns(1) // one connection: every prepared statement is on it
	t.Cleanup(func() { _ = sdb.Close() })
	if err := staleplan.Install(db); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var occ []staleplan.Occurrence
	staleplan.SetObserver(func(o staleplan.Occurrence) { mu.Lock(); occ = append(occ, o); mu.Unlock() })
	t.Cleanup(func() { staleplan.SetObserver(nil) })

	table := "sp_wasm_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, quantity int,
		created_at timestamptz NOT NULL DEFAULT NOW(), updated_at timestamptz NOT NULL DEFAULT NOW(),
		deleted_at timestamptz, created_by_id uuid)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })

	orgID := uuid.New()
	readInv := testInvocation(db, nil, orgID, stockReadEnforcer(), func(string) string { return table })
	dq := func() []byte {
		return staleplan.RetryEnvelope(db, "data_query", func() []byte {
			return executeDataQueryRecords(context.Background(), readInv, []byte(`{"table":"stock"}`))
		})
	}
	bus, _, _ := captureBus(t, "inventory.Stock.*")
	inv := testInvocation(db, bus, orgID, stockWriteEnforcer(), func(string) string { return table })
	ctx := context.Background()
	mutate := func(payload string) []byte {
		return staleplan.RetryEnvelope(db, "data_mutate", func() []byte { return executeDataMutate(ctx, inv, []byte(payload)) })
	}
	id1, id2 := uuid.NewString(), uuid.NewString()
	for _, id := range []string{id1} {
		out := mutate(`{"op":"create","table":"stock","model":"Stock","id":"` + id + `","data":{"quantity":5}}`)
		if env := unmarshalMutate(t, out); !env.Success {
			t.Fatalf("warm-up create failed: %s", out)
		}
	}
	out := mutate(`{"op":"update","table":"stock","model":"Stock","id":"` + id1 + `","data":{"quantity":6}}`)
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("warm-up update failed: %s", out)
	}

	if env := unmarshalDataQuery(t, dq()); !env.Success {
		t.Fatalf("warm-up data_query failed")
	}

	// The migration: RETURNING * / SELECT * now describe different columns.
	if err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN note text`, table)).Error; err != nil {
		t.Fatal(err)
	}

	// Same statements again. Without the layer: SQLSTATE 0A000 on every call.
	out = mutate(`{"op":"update","table":"stock","model":"Stock","id":"` + id1 + `","data":{"quantity":7}}`)
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("update after migration must heal: %s", out)
	}
	out = mutate(`{"op":"create","table":"stock","model":"Stock","id":"` + id2 + `","data":{"quantity":1}}`)
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("create after migration must heal: %s", out)
	}
	if out := dq(); !unmarshalDataQuery(t, out).Success {
		t.Fatalf("data_query after migration must heal: %s", out)
	}
	// A guest read through db_query on the same table.
	q := staleplan.RetryEnvelope(db, "db_query", func() []byte {
		return executeDBQuery(ctx, db, "inventory", nil, "SELECT * FROM "+table+" WHERE id = $1", []byte(fmt.Sprintf(`[%q]`, id1)))
	})
	if len(q) == 0 || !containsStr(string(q), `"success":true`) {
		t.Fatalf("db_query after migration: %s", q)
	}

	// Exactly-once: one row per id, the counter moved once per call.
	var n int64
	db.Raw(fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n)
	if n != 2 {
		t.Fatalf("rows = %d, want 2 (a retried write must not duplicate)", n)
	}
	var qty int
	db.Raw(fmt.Sprintf(`SELECT quantity FROM %s WHERE id = ?`, table), id1).Scan(&qty)
	if qty != 7 {
		t.Fatalf("quantity = %d, want 7", qty)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(occ) == 0 {
		t.Fatal("the migration produced no stale plan: the test is not exercising the failure")
	}
	t.Logf("stale-plan occurrences healed during the test: %d", len(occ))
	for _, o := range occ {
		t.Logf("  kind=%s op=%s outcome=%s", o.Kind, o.Op, o.Outcome)
		if o.Outcome == "failed" {
			t.Errorf("a retry failed: %+v", o)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// BEFORE: the same scenario on a PrepareStmt pool without the stale-plan layer
// reproduces the production failure: data_query's column probe
// (`SELECT * FROM <table> LIMIT 0`, the dataquery_orgscope.go site of the
// incident) fails on every call, with no recovery short of a restart.
func TestWasmDataHost_BeforeFixFailsWith0A000(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{PrepareStmt: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sdb, _ := db.DB()
	sdb.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sdb.Close() })
	table := "sp_wasm_b_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, quantity int,
		created_at timestamptz NOT NULL DEFAULT NOW(), updated_at timestamptz NOT NULL DEFAULT NOW(),
		deleted_at timestamptz, created_by_id uuid)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	orgID := uuid.New()
	inv := testInvocation(db, nil, orgID, stockReadEnforcer(), func(string) string { return table })
	run := func() []byte {
		return executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"stock"}`))
	}
	if env := unmarshalDataQuery(t, run()); !env.Success {
		t.Fatalf("warm-up failed")
	}
	db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN note text`, table))
	for i := 0; i < 3; i++ {
		out := run()
		if !containsStr(string(out), "cached plan must not change result type") {
			t.Fatalf("attempt %d: expected the 0A000 reproduction, got %s", i, out)
		}
		t.Logf("BEFORE attempt %d: %.200s", i, out)
	}
}
