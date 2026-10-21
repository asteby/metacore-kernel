package wasm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// Regresión: un guest que reclama la fila con db_exec (UPDATE ... version =
// version + 1, dentro de la transacción de la invocación) y luego la escribe
// con data_mutate se auto-bloqueaba: data_mutate abría una transacción nueva
// sobre el pool y esperaba el bloqueo de fila de la externa hasta el plazo de
// 5 s ("context deadline exceeded"). Con transacción en vuelo debe reutilizarla.
func TestDataMutatePostgres_ReusesInvocationTxAfterDBExecClaim(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	table := "dm_claim_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, quantity int,
		version int NOT NULL DEFAULT 1,
		created_at timestamptz NOT NULL DEFAULT NOW(), updated_at timestamptz NOT NULL DEFAULT NOW(),
		deleted_at timestamptz)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	orgID, rowID := uuid.New(), uuid.NewString()
	if err := db.Exec(fmt.Sprintf(`INSERT INTO %s (id, organization_id, quantity) VALUES (?, ?, 1)`, table), rowID, orgID).Error; err != nil {
		t.Fatal(err)
	}

	bus, _, _ := captureBus(t, "inventory.Stock.*")
	inv := testInvocation(db, bus, orgID, stockWriteEnforcer(), func(string) string { return table })

	tx := db.Begin()
	defer tx.Rollback()
	inv.tx = tx
	if err := tx.Exec(fmt.Sprintf(`UPDATE %s SET version = version + 1 WHERE id = ?`, table), rowID).Error; err != nil {
		t.Fatal(err)
	}

	done := make(chan []byte, 1)
	go func() {
		done <- executeDataMutate(context.Background(), inv,
			[]byte(`{"op":"update","table":"stock","model":"Stock","id":"`+rowID+`","data":{"quantity":9}}`))
	}()
	select {
	case out := <-done:
		if env := unmarshalMutate(t, out); !env.Success {
			t.Fatalf("data_mutate failed: %s", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("data_mutate se bloqueó esperando la fila tomada por la transacción de la invocación")
	}

	var q, v int
	row := tx.Raw(fmt.Sprintf(`SELECT quantity, version FROM %s WHERE id = ?`, table), rowID).Row()
	if err := row.Scan(&q, &v); err != nil || q != 9 || v != 2 {
		t.Fatalf("quantity=%d version=%d err=%v, want 9/2", q, v, err)
	}
}

// Regresión de #531: con la escritura en la transacción de la invocación (aún sin
// confirmar), data_query leía del pool y no veía la fila recién creada: la
// Storefront API creaba el carrito con data_batch y al releerlo por id respondía
// «carrito no encontrado». Las lecturas del host deben usar la misma transacción.
func TestDataQueryPostgres_SeesWritesOfTheInvocationTx(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	table := "dq_ryw_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, quantity int,
		created_at timestamptz NOT NULL DEFAULT NOW(), updated_at timestamptz NOT NULL DEFAULT NOW(),
		deleted_at timestamptz)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })

	enf := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{
			{Kind: "db:write", Target: "stock"},
			{Kind: "db:read", Target: "stock"},
		})
	})
	enf.SetMode(security.ModeEnforce)
	orgID, rowID := uuid.New(), uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.*")
	inv := testInvocation(db, bus, orgID, enf, func(string) string { return table })

	tx := db.Begin()
	defer tx.Rollback()
	inv.tx = tx

	out := executeDataMutate(context.Background(), inv,
		[]byte(`{"op":"create","table":"stock","model":"Stock","id":"`+rowID+`","data":{"quantity":5}}`))
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("data_mutate failed: %s", out)
	}

	q := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"stock","where":{"id":"`+rowID+`"}}`))
	env := unmarshalDataQuery(t, q)
	if !env.Success || env.Data == nil || len(env.Data.Rows) != 1 {
		t.Fatalf("data_query no vio la fila escrita en la transacción de la invocación: %s", q)
	}

	// Sin transacción en vuelo la lectura sigue yendo al pool y no ve lo no confirmado.
	inv2 := testInvocation(db, bus, orgID, enf, func(string) string { return table })
	q2 := executeDataQueryRecords(context.Background(), inv2, []byte(`{"table":"stock","where":{"id":"`+rowID+`"}}`))
	if env2 := unmarshalDataQuery(t, q2); env2.Data != nil && len(env2.Data.Rows) != 0 {
		t.Fatalf("sin tx en vuelo no debería ver filas sin confirmar: %s", q2)
	}
}
