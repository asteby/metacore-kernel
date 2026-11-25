package dedupe_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/idempotency"
	"github.com/asteby/metacore-kernel/support/contract"
	"github.com/asteby/metacore-kernel/support/contract/contracttest"
	"github.com/asteby/metacore-kernel/support/dedupe"
)

// Tests run against SQLite when the binary has CGO (the repo driver is
// mattn/go-sqlite3; without CGO they skip) and against Postgres when
// TEST_POSTGRES_DSN points to a scratch database, like package idempotency.

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("file:dedupe_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())
	db, err := gorm.Open(sqlite.Open(name), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		if strings.Contains(err.Error(), "CGO_ENABLED=0") {
			t.Skip("sqlite driver needs CGO: skipping")
		}
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
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres dedupe test")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	schema := "dedupe_test_" + randHex()
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	conn := dsn + " search_path=" + schema
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if u, err := url.Parse(dsn); err == nil {
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			conn = u.String()
		}
	}
	db, err := gorm.Open(postgres.Open(conn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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

func eachDialect(t *testing.T, fn func(t *testing.T, newDB func(*testing.T) *gorm.DB)) {
	t.Run("SQLite", func(t *testing.T) { fn(t, sqliteDB) })
	t.Run("Postgres", func(t *testing.T) { fn(t, postgresDB) })
}

func newStore(t *testing.T, db *gorm.DB, clock *contracttest.Clock) *idempotency.GormStore {
	t.Helper()
	s, err := idempotency.NewGormStore(db, idempotency.GormStoreOptions{CleanupInterval: -1, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGormDeduperConformance(t *testing.T) {
	eachDialect(t, func(t *testing.T, newDB func(*testing.T) *gorm.DB) {
		db := newDB(t)
		var n atomic.Int32
		contracttest.RunDedupe(t, func(t *testing.T, clock *contracttest.Clock, max int) contract.DeliveryDeduper {
			// One namespace per call isolates cases that share the table.
			d, err := dedupe.New(newStore(t, db, clock), dedupe.Options{
				Namespace: fmt.Sprintf("conf-%d", n.Add(1)), MaxEntries: max,
			})
			if err != nil {
				t.Fatal(err)
			}
			return d
		})
	})
}

// Two stores on one database model two replicas: still exactly one winner.
func TestGormDeduperAcrossReplicas(t *testing.T) {
	eachDialect(t, func(t *testing.T, newDB func(*testing.T) *gorm.DB) {
		db := newDB(t)
		clock := contracttest.NewClock()
		const replicas, ids = 4, 25
		ds := make([]*dedupe.Deduper, replicas)
		for i := range ds {
			d, err := dedupe.New(newStore(t, db, clock), dedupe.Options{Namespace: "support-callback"})
			if err != nil {
				t.Fatal(err)
			}
			ds[i] = d
		}
		wins := make([]atomic.Int32, ids)
		var wg sync.WaitGroup
		for _, d := range ds {
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func(d *dedupe.Deduper) {
					defer wg.Done()
					for i := 0; i < ids; i++ {
						first, err := d.FirstSeen(context.Background(), fmt.Sprintf("m-%d", i), contract.DedupeTTL)
						if err != nil {
							t.Errorf("FirstSeen: %v", err)
							return
						}
						if first {
							wins[i].Add(1)
						}
					}
				}(d)
			}
		}
		wg.Wait()
		for i := range wins {
			if n := wins[i].Load(); n != 1 {
				t.Fatalf("id m-%d had %d winners; want 1", i, n)
			}
		}
	})
}

func TestGormDeduperNamespacesForgetPurge(t *testing.T) {
	eachDialect(t, func(t *testing.T, newDB func(*testing.T) *gorm.DB) {
		ctx := context.Background()
		clock := contracttest.NewClock()
		store := newStore(t, newDB(t), clock)
		a, _ := dedupe.New(store, dedupe.Options{Namespace: "support-callback"})
		b, _ := dedupe.New(store, dedupe.Options{Namespace: "other_consumer"})

		if first, _ := a.FirstSeen(ctx, "id", time.Hour); !first {
			t.Fatal("a: first")
		}
		if first, _ := b.FirstSeen(ctx, "id", time.Hour); !first {
			t.Fatal("same id in another namespace must be first")
		}
		// The cap of one namespace ignores the other one's rows.
		c, _ := dedupe.New(store, dedupe.Options{Namespace: "support-callback-2", MaxEntries: 1})
		if first, err := c.FirstSeen(ctx, "id", time.Hour); err != nil || !first {
			t.Fatalf("c first = %v, %v", first, err)
		}

		// Forget lets the redelivery through; Confirm keeps the record.
		a.Forget(ctx, "id")
		if first, _ := a.FirstSeen(ctx, "id", time.Hour); !first {
			t.Fatal("after Forget the id must be first again")
		}
		a.Confirm("id")
		a.Forget(ctx, "id") // no token left: no-op
		if first, _ := a.FirstSeen(ctx, "id", time.Hour); first {
			t.Fatal("after Confirm the id must stay a duplicate")
		}

		clock.Advance(2 * time.Hour)
		n, err := a.Purge(ctx)
		if err != nil || n != 3 {
			t.Fatalf("Purge = %d, %v; want 3 expired rows", n, err)
		}
	})
}

func TestGormDeduperTTLOverrideAndValidation(t *testing.T) {
	db := sqliteDB(t)
	clock := contracttest.NewClock()
	store := newStore(t, db, clock)
	ctx := context.Background()

	d, err := dedupe.New(store, dedupe.Options{Namespace: "ns", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	d.FirstSeen(ctx, "x", 24*time.Hour) // the override wins over the caller's ttl
	clock.Advance(2 * time.Minute)
	if first, _ := d.FirstSeen(ctx, "x", 24*time.Hour); !first {
		t.Fatal("Options.TTL must override the caller ttl")
	}

	for _, bad := range []dedupe.Options{{}, {Namespace: "a b"}, {Namespace: "a%"}, {Namespace: "ok", MaxEntries: -1}} {
		if _, err := dedupe.New(store, bad); err == nil {
			t.Fatalf("New(%+v) should fail", bad)
		}
	}
	if _, err := dedupe.New(nil, dedupe.Options{Namespace: "ns"}); err == nil {
		t.Fatal("nil store should fail")
	}
}

func TestGormDeduperErrDedupeFullIsContractError(t *testing.T) {
	db := sqliteDB(t)
	clock := contracttest.NewClock()
	d, _ := dedupe.New(newStore(t, db, clock), dedupe.Options{Namespace: "ns", MaxEntries: 1})
	d.FirstSeen(context.Background(), "a", time.Hour)
	if _, err := d.FirstSeen(context.Background(), "b", time.Hour); !errors.Is(err, contract.ErrDedupeFull) {
		t.Fatalf("err = %v", err)
	}
}
