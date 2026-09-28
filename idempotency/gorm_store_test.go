package idempotency_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/idempotency"
)

// The GormStore tests run against SQLite always and against Postgres when
// TEST_POSTGRES_DSN points to a scratch database (each test gets its own
// schema, dropped on cleanup).

func sqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("file:idem_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())
	db, err := gorm.Open(sqlite.Open(name), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres idempotency test")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	schema := "idem_test_" + randHex()
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.Open(withSearchPath(dsn, schema)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func withSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
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

func newStore(t *testing.T, db *gorm.DB, now func() time.Time) *idempotency.GormStore {
	t.Helper()
	s, err := idempotency.NewGormStore(db, idempotency.GormStoreOptions{CleanupInterval: -1, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGormStore_GetPutAndExpiry(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		s := newStore(t, db, nil)
		if _, ok := s.Get("k"); ok {
			t.Fatal("unknown key should miss")
		}
		s.Put("k", idempotency.Stored{StatusCode: 201, Body: []byte(`{"id":1}`), ContentType: "application/json", ExpiresAt: time.Now().Add(time.Hour)})
		got, ok := s.Get("k")
		if !ok || got.StatusCode != 201 || string(got.Body) != `{"id":1}` || got.ContentType != "application/json" {
			t.Fatalf("get after put = %+v, %v", got, ok)
		}
		s.Put("old", idempotency.Stored{StatusCode: 200, ExpiresAt: time.Now().Add(-time.Minute)})
		if _, ok := s.Get("old"); ok {
			t.Fatal("expired entry should miss")
		}
		n, err := s.Cleanup(context.Background())
		if err != nil || n != 1 {
			t.Fatalf("cleanup = %d, %v; want 1 row", n, err)
		}
		// Migrate is idempotent (second store on the same DB).
		newStore(t, db, nil)
	})
}

func TestGormStore_ReserveLifecycle(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		now := func() time.Time { return time.Unix(0, clock.Load()) }
		s := newStore(t, db, now)
		ctx := context.Background()

		r1, err := s.Reserve(ctx, "k", time.Minute)
		if err != nil || !r1.Acquired || r1.Token == "" {
			t.Fatalf("first reserve = %+v, %v", r1, err)
		}
		r2, err := s.Reserve(ctx, "k", time.Minute)
		if err != nil || !r2.InFlight() {
			t.Fatalf("second reserve should be in flight, got %+v, %v", r2, err)
		}
		if _, ok := s.Get("k"); ok {
			t.Fatal("pending reservation must read as a miss")
		}

		// Release with a stale token does nothing; with the right one frees the key.
		if err := s.Release(ctx, "k", "not-the-token"); err != nil {
			t.Fatal(err)
		}
		if r, _ := s.Reserve(ctx, "k", time.Minute); !r.InFlight() {
			t.Fatalf("stale-token release must not free the key: %+v", r)
		}
		if err := s.Release(ctx, "k", r1.Token); err != nil {
			t.Fatal(err)
		}
		r3, err := s.Reserve(ctx, "k", time.Minute)
		if err != nil || !r3.Acquired {
			t.Fatalf("reserve after release = %+v, %v", r3, err)
		}

		// Completing stores the response; later reserves replay it.
		s.Put("k", idempotency.Stored{StatusCode: 201, Body: []byte("ok"), ExpiresAt: now().Add(time.Hour)})
		r4, err := s.Reserve(ctx, "k", time.Minute)
		if err != nil || r4.Replay == nil || string(r4.Replay.Body) != "ok" {
			t.Fatalf("reserve after put should replay, got %+v, %v", r4, err)
		}

		// A pending claim whose lease expired can be taken over.
		if r, _ := s.Reserve(ctx, "lease", time.Second); !r.Acquired {
			t.Fatal("lease: first reserve should acquire")
		}
		clock.Add(int64(2 * time.Second))
		if r, _ := s.Reserve(ctx, "lease", time.Second); !r.Acquired {
			t.Fatalf("expired lease should be reclaimable, got %+v", r)
		}
	})
}

func TestGormStore_ConcurrentDuplicate_ExecutesOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		for _, wait := range []time.Duration{0, 5 * time.Second} {
			t.Run(fmt.Sprintf("InFlightWait=%s", wait), func(t *testing.T) {
				store := newStore(t, db, nil)
				var calls atomic.Int32
				app := fiber.New()
				app.Use(idempotency.Middleware(idempotency.Config{Store: store, InFlightWait: wait}))
				app.Post("/orders", func(c fiber.Ctx) error {
					n := calls.Add(1)
					time.Sleep(300 * time.Millisecond)
					return c.Status(201).JSON(fiber.Map{"order": n})
				})

				key := "checkout-" + randHex()
				const n = 8
				type result struct {
					status int
					body   string
					replay string
				}
				results := make([]result, n)
				var wg sync.WaitGroup
				for i := range n {
					wg.Go(func() {
						req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader("{}"))
						req.Header.Set(idempotency.HeaderKey, key)
						resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
						if err != nil {
							t.Error(err)
							return
						}
						defer resp.Body.Close()
						results[i] = result{resp.StatusCode, readAll(resp), resp.Header.Get("Idempotent-Replay")}
					})
				}
				wg.Wait()

				if got := calls.Load(); got != 1 {
					t.Fatalf("handler ran %d times, want exactly 1", got)
				}
				created, conflicts := 0, 0
				for _, r := range results {
					switch r.status {
					case 201:
						created++
						if r.body != `{"order":1}` {
							t.Fatalf("unexpected body %s", r.body)
						}
					case 409:
						conflicts++
					default:
						t.Fatalf("unexpected status %d body %s", r.status, r.body)
					}
				}
				if wait == 0 && (created != 1 || conflicts != n-1) {
					t.Fatalf("no wait: want 1 created + %d conflicts, got %d + %d", n-1, created, conflicts)
				}
				if wait > 0 && created != n {
					t.Fatalf("with wait: all %d should get the (replayed) 201, got %d", n, created)
				}
			})
		}
	})
}

func TestGormStore_FailedRequestReleasesKey(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		store := newStore(t, db, nil)
		var calls atomic.Int32
		app := fiber.New()
		app.Use(idempotency.Middleware(idempotency.Config{Store: store}))
		app.Post("/pay", func(c fiber.Ctx) error {
			if calls.Add(1) == 1 {
				return c.Status(502).SendString("gateway down")
			}
			return c.Status(201).SendString("paid")
		})
		send := func() int {
			req := httptest.NewRequest(http.MethodPost, "/pay", nil)
			req.Header.Set(idempotency.HeaderKey, "pay-1")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		if s := send(); s != 502 {
			t.Fatalf("first = %d", s)
		}
		if s := send(); s != 201 {
			t.Fatalf("retry after failure should run the handler again, got %d", s)
		}
		if s := send(); s != 201 || calls.Load() != 2 {
			t.Fatalf("third should replay: status %d calls %d", s, calls.Load())
		}
	})
}

func readAll(resp *http.Response) string {
	var sb strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}
