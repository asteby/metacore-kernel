package wasm

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/dynamic"
)

// data_mutate over a table that pre-dates the audit standard (no updated_by_id /
// deleted_by_id): create / update / soft delete must succeed, stamping only the
// audit columns the table has. Needs TEST_POSTGRES_DSN (skips otherwise).
func TestDataMutatePostgres_LegacyTableWithoutAuditColumns(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	table := "dm_legacy_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, quantity int,
		created_at timestamptz NOT NULL DEFAULT NOW(), updated_at timestamptz NOT NULL DEFAULT NOW(),
		deleted_at timestamptz, created_by_id uuid)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })

	orgID, rowID, actor := uuid.New(), uuid.NewString(), uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.*")
	inv := testInvocation(db, bus, orgID, stockWriteEnforcer(), func(string) string { return table })
	ctx := dynamic.WithActorID(context.Background(), actor)
	run := func(payload string) {
		t.Helper()
		out := executeDataMutate(ctx, inv, []byte(payload))
		if env := unmarshalMutate(t, out); !env.Success {
			t.Fatalf("data_mutate failed: %s", out)
		}
	}
	run(`{"op":"create","table":"stock","model":"Stock","id":"` + rowID + `","data":{"quantity":5}}`)
	run(`{"op":"update","table":"stock","model":"Stock","id":"` + rowID + `","data":{"quantity":7}}`)
	run(`{"op":"delete","table":"stock","model":"Stock","id":"` + rowID + `"}`)

	row := map[string]any{}
	if err := db.Raw(fmt.Sprintf(`SELECT * FROM %s WHERE id = ?`, table), rowID).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row["created_by_id"] != actor {
		t.Errorf("created_by_id = %v, want %s", row["created_by_id"], actor)
	}
	if row["deleted_at"] == nil {
		t.Errorf("row was not soft-deleted")
	}
}
