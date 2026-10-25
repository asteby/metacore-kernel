package diag

import (
	"context"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/asteby/metacore-kernel/migrations"
)

var ws = regexp.MustCompile(`\s+`)

func norm(s string) string { return strings.TrimSpace(ws.ReplaceAllString(s, " ")) }

// El DDL de la migracion goose 0009 y Schema("postgres") no pueden divergir.
func TestMigration0009MatchesSchema(t *testing.T) {
	raw, err := os.ReadFile("../migrations/sqlfiles/0009_addon_diagnostics.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := norm(strings.Split(string(raw), "-- +goose Down")[0])
	for _, q := range Schema("postgres") {
		if !strings.Contains(up, norm(q)) {
			t.Errorf("0009 does not contain:\n%s", q)
		}
	}
	legacy, err := os.ReadFile("../migrations/sql/0009_addon_diagnostics.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if norm(strings.Split(string(raw), "-- +goose Down")[0]) != norm(string(legacy)) {
		t.Error("migrations/sql up differs from migrations/sqlfiles")
	}
}

// El runner de goose (PG real) crea tablas utilizables por el Store; Down las quita.
func TestMigration0009_PostgresRunner(t *testing.T) {
	db := pgSchema(t)
	ctx := context.Background()
	// 0001..0007 son DDL SQLite del kernel (los hosts PG migran lo suyo); aqui
	// se aplica solo 0009 con goose contra Postgres real.
	body, err := fs.ReadFile(migrations.SQLFiles, "sqlfiles/0009_addon_diagnostics.sql")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fstest.MapFS{"0009_addon_diagnostics.sql": {Data: body}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	rec, st := mkRec(t, db, nil, Options{})
	org := uuid.New()
	rec.Record(failure(org), nil)
	rec.Flush(ctx)
	if _, total, err := st.List(ctx, Filter{Org: org}); err != nil || total != 1 {
		t.Fatalf("total=%d err=%v", total, err)
	}
	var typ string
	if err := db.Raw("SELECT DISTINCT jsonb_typeof(payload) FROM " + TableEvents).Scan(&typ).Error; err != nil || typ != "object" {
		t.Fatalf("jsonb_typeof=%q err=%v", typ, err)
	}
	// El flujo ya migrado y Migrate() conviven (idempotente).
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var n int
	db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'addon_diagnostic%'").Scan(&n)
	if n != 0 {
		t.Fatalf("tables left after Down: %d", n)
	}
}
