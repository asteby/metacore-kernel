//go:build integration

package migrations_test

// Integration test for Postgres dialect.
// Run with: go test -tags integration -run TestRunnerDialect_Postgres ./migrations/...
// Requires: TEST_POSTGRES_DSN env var pointing to a running Postgres instance.
//
// Example:
//   TEST_POSTGRES_DSN="host=localhost user=postgres password=postgres dbname=testdb sslmode=disable" \
//     go test -tags integration ./migrations/...

import (
	"context"
	"os"
	"testing"

	"github.com/pressly/goose/v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/migrations"
)

func TestRunnerDialect_Postgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("skipping Postgres integration test: TEST_POSTGRES_DSN not set")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}

	r := migrations.Runner{
		Dialect:   goose.DialectPostgres,
		TableName: "goose_db_version_test",
	}
	ctx := context.Background()

	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}

	list, err := r.Status(ctx, db)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, m := range list {
		if !m.Applied {
			t.Errorf("migration %d should be applied after Up", m.Version)
		}
	}

	// Cleanup.
	if err := r.Down(ctx, db, 0); err != nil {
		t.Fatalf("Down: %v", err)
	}
}
