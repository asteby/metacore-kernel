package migrations_test

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"

	_ "github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/migrations"
)

func openInMemory(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite3: %v", err)
	}
	return db
}

func TestRunnerUp(t *testing.T) {
	db := openInMemory(t)
	r := migrations.Runner{}
	ctx := context.Background()

	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}

	sqlDB, _ := db.DB()
	for _, table := range []string{
		"users", "organizations", "webhooks", "webhook_deliveries",
		"push_subscriptions", "metacore_installations",
	} {
		var name string
		row := sqlDB.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
		if err := row.Scan(&name); err != nil {
			t.Errorf("table %q not found after Up: %v", table, err)
		}
	}
}

func TestRunnerStatus(t *testing.T) {
	db := openInMemory(t)
	r := migrations.Runner{}
	ctx := context.Background()

	// Before Up: all migrations should be unapplied.
	list, err := r.Status(ctx, db)
	if err != nil {
		t.Fatalf("Status (before Up): %v", err)
	}
	for _, m := range list {
		if m.Applied {
			t.Errorf("migration %d/%q should not be applied before Up", m.Version, m.Name)
		}
	}

	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// After Up: all should be applied.
	list, err = r.Status(ctx, db)
	if err != nil {
		t.Fatalf("Status (after Up): %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected at least one migration in Status")
	}
	for _, m := range list {
		if !m.Applied {
			t.Errorf("migration %d/%q should be applied after Up", m.Version, m.Name)
		}
	}
}

func TestRunnerDownSteps(t *testing.T) {
	db := openInMemory(t)
	r := migrations.Runner{}
	ctx := context.Background()

	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Roll back 1 step.
	if err := r.Down(ctx, db, 1); err != nil {
		t.Fatalf("Down(1): %v", err)
	}

	list, err := r.Status(ctx, db)
	if err != nil {
		t.Fatalf("Status after Down(1): %v", err)
	}
	last := list[len(list)-1]
	if last.Applied {
		t.Errorf("last migration %d should be rolled back after Down(1)", last.Version)
	}
}

func TestRunnerUpIdempotent(t *testing.T) {
	db := openInMemory(t)
	r := migrations.Runner{}
	ctx := context.Background()

	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("second Up (idempotent): %v", err)
	}
}

// TestRunnerDialect_SQLite3Explicit verifies that an explicit SQLite3 dialect
// produces the same behaviour as the zero-value default.
func TestRunnerDialect_SQLite3Explicit(t *testing.T) {
	db := openInMemory(t)
	r := migrations.Runner{Dialect: goose.DialectSQLite3}
	ctx := context.Background()

	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("Up with explicit SQLite3 dialect: %v", err)
	}

	list, err := r.Status(ctx, db)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, m := range list {
		if !m.Applied {
			t.Errorf("migration %d should be applied", m.Version)
		}
	}
}
