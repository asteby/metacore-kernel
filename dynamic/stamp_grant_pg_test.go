package dynamic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Una columna grant uuid real: el patch de un update que suelta el grant gastado
// debe aplicarse en Postgres (con "" fallaba: invalid input syntax for uuid).
func TestPostgres_StampUpdateClearsSpentGrantUUID(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	schema := "stg_" + hex.EncodeToString(b)
	if err := db.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	if err := db.Exec(`SET search_path TO ` + schema).Error; err != nil {
		t.Fatal(err)
	}
	// Una sola conexión para que el search_path aplique a todo.
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec(`CREATE TABLE stamp_lines (id uuid PRIMARY KEY, unit_price numeric(18,4), price_authorized numeric(18,4), price_approval_id uuid)`).Error; err != nil {
		t.Fatal(err)
	}
	st := NewModelStamps([]manifest.ColumnDef{
		{Name: "price_approval_id", Grant: &manifest.GrantDef{
			Policy: "p", Sets: "price_authorized",
			ContextGte: map[string]string{"short": "50 - unit_price"},
		}},
	}, nil, nil)
	line, spent := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO stamp_lines VALUES (?,?,?,?)`, line, 45, 1, spent).Error; err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	if err := db.Table("stamp_lines").Where("id = ?", line).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	// stamp_lines no tiene price_approval_id relevante sin Relevant: se toca unit_price.
	patch := map[string]any{"unit_price": 60.0}
	consume := func(context.Context, ConsumeInput) error { return nil }
	if _, err := st.prepareUpdate(context.Background(), db, uuid.New(), uuid.New(), "lines", line.String(), before, patch, nil, consume); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("stamp_lines").Where("id = ?", line).Updates(patch).Error; err != nil {
		t.Fatalf("update con grant gastado: %v", err)
	}
	var got *string
	if err := db.Raw(`SELECT price_approval_id::text FROM stamp_lines WHERE id = ?`, line).Scan(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("price_approval_id = %q, want NULL", *got)
	}
}
