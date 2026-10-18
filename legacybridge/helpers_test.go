package legacybridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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

// sqliteDB abre una SQLite en memoria con una sola conexión (como las pruebas del outbox).
func sqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:legacybridge_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// postgresDB usa TEST_POSTGRES_DSN con un esquema propio por prueba.
func postgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN no definido")
	}
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatal(err)
	}
	schema := "legacybridge_test_" + randHex()
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s, err := db.DB(); err == nil {
			_ = s.Close()
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if s, err := admin.DB(); err == nil {
			_ = s.Close()
		}
	})
	return db
}

func gormStore(t *testing.T, db *gorm.DB) *GormStore {
	t.Helper()
	s, err := NewGormStore(context.Background(), db, true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeSink es un destino en memoria que registra lo que escribe.
type fakeSink struct {
	mu      sync.Mutex
	rows    map[string]map[string]any // entity|local -> root
	deleted map[string]bool
	writes  []Write
	fail    error
}

func newFakeSink() *fakeSink {
	return &fakeSink{rows: map[string]map[string]any{}, deleted: map[string]bool{}}
}

func (f *fakeSink) DeriveID(org, entity, ext string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(org+"|"+entity+"|"+ext)).String()
}

func (f *fakeSink) Exists(_ context.Context, _, entity, local string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[entity+"|"+local]
	return ok && !f.deleted[entity+"|"+local], nil
}

func (f *fakeSink) Upsert(_ context.Context, w Write) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.rows[w.Envelope.Entity+"|"+w.LocalID] = w.Envelope.Aggregate.Root
	for i, fc := range w.Envelope.Facets {
		f.rows[fc.Entity+"|"+w.FacetIDs[i]] = fc.Row
	}
	delete(f.deleted, w.Envelope.Entity+"|"+w.LocalID)
	f.writes = append(f.writes, w)
	return nil
}

func (f *fakeSink) Delete(_ context.Context, w Write) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[w.Envelope.Entity+"|"+w.LocalID] = true
	f.writes = append(f.writes, w)
	return nil
}

// seed crea una fila local sin bridge_refs (como la carga inicial).
func (f *fakeSink) seed(org, entity, ext string) string {
	id := f.DeriveID(org, entity, ext)
	f.mu.Lock()
	f.rows[entity+"|"+id] = map[string]any{"seed": true}
	f.mu.Unlock()
	return id
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func ver(sec int, hash string) string {
	return time.Date(2026, 10, 18, 10, 0, sec, 0, time.UTC).Format(time.RFC3339Nano) + "#" + hash
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
