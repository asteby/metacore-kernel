package diag

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// eachDialect corre fn en SQLite (memoria) y, con TEST_POSTGRES_DSN, en un
// esquema desechable de Postgres.
func eachDialect(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Run("SQLite", func(t *testing.T) {
		db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:diag_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())),
			&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		s, _ := db.DB()
		s.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = s.Close() })
		if err := Migrate(t.Context(), db); err != nil {
			t.Fatal(err)
		}
		fn(t, db)
	})
	t.Run("Postgres", func(t *testing.T) {
		db := pgSchema(t)
		if err := Migrate(t.Context(), db); err != nil {
			t.Fatal(err)
		}
		fn(t, db)
	})
}

func pgSchema(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatal(err)
	}
	schema := "diag_test_" + randHex()
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	var conn string
	if err == nil && u.Scheme != "" {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		conn = u.String()
	} else {
		conn = dsn + " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(conn), cfg)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(40)
	t.Cleanup(func() {
		_ = sqlDB.Close()
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if a, e := admin.DB(); e == nil {
			_ = a.Close()
		}
	})
	return db
}

// clock es un reloj de pruebas.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
