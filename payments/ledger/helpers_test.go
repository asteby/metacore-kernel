package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Every DB test runs on SQLite and, when TEST_POSTGRES_DSN is set, on
// Postgres (own schema per test). `-run /Postgres` selects the latter.

func sqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:ledger_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func postgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres ledger test")
	}
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	schema := "ledger_test_" + randHex()
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.Open(withSearchPath(dsn, schema)), cfg)
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

func withSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if u, err := url.Parse(dsn); err == nil {
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return dsn + " search_path=" + schema
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func eachDialect(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Run("SQLite", func(t *testing.T) { fn(t, sqliteDB(t)) })
	t.Run("Postgres", func(t *testing.T) { fn(t, postgresDB(t)) })
}

// fakeProviders answers commerce.payments.* per (provider, capability).
type fakeProviders struct {
	mu       sync.Mutex
	handlers map[string]func(in map[string]any) (any, error)
	calls    []capability.Query
}

func newFake() *fakeProviders {
	return &fakeProviders{handlers: map[string]func(map[string]any) (any, error){}}
}

func (f *fakeProviders) on(provider, key string, h func(in map[string]any) (any, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[provider+"|"+key] = h
}

func (f *fakeProviders) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Capability == key {
			n++
		}
	}
	return n
}

func (f *fakeProviders) Query(_ context.Context, q capability.Query) (capability.QueryResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, q)
	h := f.handlers[q.Provider+"|"+q.Capability]
	f.mu.Unlock()
	res := capability.QueryResult{Capability: q.Capability}
	if h == nil {
		return res, fmt.Errorf("%w: %s %s", capability.ErrNoProvider, q.Provider, q.Capability)
	}
	out, err := h(q.Input)
	r := capability.Result{Provider: q.Provider, Err: err}
	if err == nil {
		r.Output, _ = json.Marshal(out)
	}
	res.Results = []capability.Result{r}
	return res, nil
}

type published struct {
	emitter, event string
	payload        map[string]any
}

type harness struct {
	l         *Ledger
	fake      *fakeProviders
	org       uuid.UUID
	now       time.Time
	mu        sync.Mutex
	events    []published
	anomalies []Anomaly
}

func newHarness(t *testing.T, db *gorm.DB) *harness {
	t.Helper()
	h := &harness{fake: newFake(), org: uuid.New(), now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	l, err := New(context.Background(), db, Config{
		Providers: h.fake,
		Publish: func(_ context.Context, emitter, event string, _ uuid.UUID, payload any) error {
			var m map[string]any
			b, _ := json.Marshal(payload)
			_ = json.Unmarshal(b, &m)
			h.mu.Lock()
			h.events = append(h.events, published{emitter, event, m})
			h.mu.Unlock()
			return nil
		},
		Anomalies: AnomalySinkFunc(func(_ context.Context, a Anomaly) error {
			h.mu.Lock()
			h.anomalies = append(h.anomalies, a)
			h.mu.Unlock()
			return nil
		}),
		IsPaymentsProvider: func(_ context.Context, _ uuid.UUID, addon string) (bool, error) {
			return addon == "mercadopago" || addon == "clip", nil
		},
		Now: func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Migrate is idempotent.
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	h.l = l
	return h
}

func (h *harness) confirmed() []published {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []published
	for _, e := range h.events {
		if e.event == capability.CommercePaymentsConfirmed {
			out = append(out, e)
		}
	}
	return out
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

// statusOf builds a commerce.payments.status answer.
func statusOf(ref, status, amount string) map[string]any {
	return map[string]any{"order_ref": "SO-1", "status": status, "amount": amount, "currency": "MXN", "provider_ref": ref}
}

func timeoutErr() error {
	return fmt.Errorf("%w: %w: provider slow", capability.ErrTimeout, capability.ErrIndeterminate)
}
