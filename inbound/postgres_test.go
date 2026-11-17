package inbound

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Receipt-store tests run on SQLite (when CGO is available) and on Postgres
// when TEST_POSTGRES_DSN points to a scratch database (CI job "postgres"; each
// test gets its own schema). Postgres is the one that proves the reservation
// is atomic across real concurrent connections: SQLite is capped to a single
// connection and cannot.

func postgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres inbound test")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	rnd := make([]byte, 6)
	_, _ = rand.Read(rnd)
	schema := "inbound_test_" + hex.EncodeToString(rnd)
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.Open(withSearchPath(dsn, schema)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(32)
	t.Cleanup(func() {
		_ = sqlDB.Close()
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if a, err := admin.DB(); err == nil {
			_ = a.Close()
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

func eachReceiptDB(t *testing.T, fn func(t *testing.T, st *GormReceiptStore)) {
	mk := func(db *gorm.DB) *GormReceiptStore {
		st, err := NewGormReceiptStore(db)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	t.Run("SQLite", func(t *testing.T) { fn(t, mk(openSQLite(t))) })
	t.Run("Postgres", func(t *testing.T) { fn(t, mk(postgresDB(t))) })
}

func TestReceiptStoreAtomicReservation(t *testing.T) {
	eachReceiptDB(t, func(t *testing.T, st *GormReceiptStore) {
		ctx := context.Background()
		org := uuid.New()
		r := Receipt{OrgID: org, Contract: "stock.changed", Version: 1, KeyHash: HashKey(org, "stock.changed", "k"), BodyHash: "b"}

		// 24 racers, one key: exactly one wins the reservation.
		var wg sync.WaitGroup
		var won, errs int32
		start := make(chan struct{})
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, c, err := st.Begin(ctx, r, time.Minute); err != nil {
					atomic.AddInt32(&errs, 1)
				} else if c {
					atomic.AddInt32(&won, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if won != 1 || errs != 0 {
			t.Fatalf("won=%d errs=%d, want exactly one claimant", won, errs)
		}
	})
}

func TestReceiptStoreStaleReclaimRaceAndFencing(t *testing.T) {
	eachReceiptDB(t, func(t *testing.T, st *GormReceiptStore) {
		ctx := context.Background()
		org := uuid.New()
		r := Receipt{OrgID: org, Contract: "c", Version: 1, KeyHash: HashKey(org, "c", "k"), BodyHash: "b"}
		slow, claimed, err := st.Begin(ctx, r, time.Minute)
		if err != nil || !claimed {
			t.Fatal(err, claimed)
		}
		time.Sleep(30 * time.Millisecond)

		// The lease (10 ms) expired: 16 racers try to take over, one wins.
		var wg sync.WaitGroup
		var won int32
		var mu sync.Mutex
		var winner Receipt
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if cur, c, err := st.Begin(ctx, r, 10*time.Millisecond); err == nil && c {
					atomic.AddInt32(&won, 1)
					mu.Lock()
					winner = cur
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if won != 1 || winner.Attempts != 2 {
			t.Fatalf("reclaim winners=%d attempts=%d", won, winner.Attempts)
		}

		// Fencing: the slow first attempt cannot overwrite the new owner.
		if ok, err := st.FinishAttempt(ctx, slow.ID, slow.Attempts, StatusFailed, "", "internal"); err != nil || ok {
			t.Fatalf("stale attempt finished: ok=%v err=%v", ok, err)
		}
		if ok, err := st.FinishAttempt(ctx, winner.ID, winner.Attempts, StatusApplied, "rec", ""); err != nil || !ok {
			t.Fatalf("owner could not finish: ok=%v err=%v", ok, err)
		}
		got, _ := st.List(ctx, org, ReceiptFilter{})
		if len(got) != 1 || got[0].Status != StatusApplied || got[0].RecordID != "rec" {
			t.Fatalf("%+v", got)
		}
		// Finished receipts are not fence-finishable twice.
		if ok, _ := st.FinishAttempt(ctx, winner.ID, winner.Attempts, StatusFailed, "", "x"); ok {
			t.Fatal("finished receipt re-finished")
		}
	})
}

func TestReceiptStoreMaxLengthKeyAndRejectedCapAndListRules(t *testing.T) {
	eachReceiptDB(t, func(t *testing.T, st *GormReceiptStore) {
		ctx := context.Background()
		org := uuid.New()
		contract := strings.Repeat("a", 128)
		r := Receipt{OrgID: org, Contract: contract, Version: 1, KeyHash: HashKey(org, contract, "k"), BodyHash: "b"}
		if _, c, err := st.Begin(ctx, r, time.Minute); err != nil || !c {
			t.Fatalf("max-length claim rejected by the column: %v %v", c, err)
		}

		st.MaxRejected = 3
		for i := 0; i < 8; i++ {
			if err := st.Record(ctx, Receipt{OrgID: org, Contract: "c", Status: StatusRejected, ErrorCode: "invalid_schema", BodyHash: "x"}); err != nil {
				t.Fatal(err)
			}
		}
		other := uuid.New()
		_ = st.Record(ctx, Receipt{OrgID: other, Contract: "c", Status: StatusRejected, BodyHash: "x"})
		rej, _ := st.List(ctx, org, ReceiptFilter{Status: StatusRejected})
		orej, _ := st.List(ctx, other, ReceiptFilter{Status: StatusRejected})
		all, _ := st.List(ctx, org, ReceiptFilter{})
		if len(rej) != 3 || len(orej) != 1 || len(all) != 4 { // 3 rejected + the claimed one
			t.Fatalf("rejected=%d other=%d all=%d", len(rej), len(orej), len(all))
		}

		if _, err := st.List(ctx, org, ReceiptFilter{EventKey: "k"}); err != ErrEventKeyNeedsContract {
			t.Fatalf("EventKey without Contract: %v", err)
		}
		byKey, err := st.List(ctx, org, ReceiptFilter{Contract: contract, EventKey: "k"})
		if err != nil || len(byKey) != 1 {
			t.Fatalf("by key: %d %v", len(byKey), err)
		}
	})
}
