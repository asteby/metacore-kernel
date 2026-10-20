package inbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/payments/incidents"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Every DB test runs on SQLite and, when TEST_POSTGRES_DSN is set, on
// Postgres (own schema per test). `-run /Postgres` selects the latter.

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:inbox_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres inbox test")
	}
	return dsn
}

// postgresDB returns a DB on a fresh schema plus a function that opens
// another connection to the same schema with extra DSN options.
func postgresDB(t *testing.T) *gorm.DB {
	db, _ := postgresDBWith(t)
	return db
}

func postgresDBWith(t *testing.T) (*gorm.DB, func(opts string) *gorm.DB) {
	t.Helper()
	dsn := postgresDSN(t)
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	schema := "inbox_test_" + randHex()
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	open := func(opts string) *gorm.DB {
		d := withSearchPath(dsn, schema, opts)
		db, err := gorm.Open(postgres.Open(d), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if s, err := db.DB(); err == nil {
				_ = s.Close()
			}
		})
		return db
	}
	db := open("")
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if s, err := admin.DB(); err == nil {
			_ = s.Close()
		}
	})
	return db, open
}

func withSearchPath(dsn, schema, opts string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			q := u.Query()
			q.Set("search_path", schema)
			if k, v, ok := strings.Cut(opts, "="); ok {
				q.Set(k, v) // runtime parameter
			}
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	out := dsn + " search_path=" + schema
	if opts != "" {
		out += " " + opts
	}
	return out
}

func eachDialect(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Run("SQLite", func(t *testing.T) { fn(t, sqliteDB(t)) })
	t.Run("Postgres", func(t *testing.T) { fn(t, postgresDB(t)) })
}

func newStores(t *testing.T, db *gorm.DB, cfg Config) (*Store, *incidents.Store) {
	t.Helper()
	inc, err := incidents.New(t.Context(), db, incidents.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Incidents = inc
	st, err := New(t.Context(), db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st, inc
}
