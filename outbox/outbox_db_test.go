package outbox

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

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Every DB test runs against SQLite (always) and Postgres when
// TEST_POSTGRES_DSN points to a scratch database; each Postgres test gets
// its own schema, dropped on cleanup. `go test -run /Postgres ./outbox/`
// selects only the Postgres variants.

type testDB struct {
	name string
	// open returns a new connection pool to the same database (Postgres)
	// or the same pool (SQLite, whose in-memory DB lives in one conn).
	open func(t *testing.T) *gorm.DB
}

func sqliteTestDB(t *testing.T) testDB {
	t.Helper()
	dsn := fmt.Sprintf("file:outbox_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return testDB{name: "sqlite", open: func(*testing.T) *gorm.DB { return db }}
}

func postgresTestDB(t *testing.T) testDB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres outbox test")
	}
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	schema := "outbox_test_" + randHex()
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	var pools []*gorm.DB
	t.Cleanup(func() {
		for _, p := range pools {
			if s, err := p.DB(); err == nil {
				_ = s.Close()
			}
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if s, err := admin.DB(); err == nil {
			_ = s.Close()
		}
	})
	return testDB{name: "postgres", open: func(t *testing.T) *gorm.DB {
		db, err := gorm.Open(postgres.Open(withSearchPath(dsn, schema)), cfg)
		if err != nil {
			t.Fatal(err)
		}
		pools = append(pools, db)
		return db
	}}
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

func eachDialect(t *testing.T, fn func(t *testing.T, tdb testDB)) {
	t.Run("SQLite", func(t *testing.T) { fn(t, sqliteTestDB(t)) })
	t.Run("Postgres", func(t *testing.T) { fn(t, postgresTestDB(t)) })
}

// fastConfig keeps the tests quick: short polls, tiny fixed backoff.
func fastConfig(c Config) Config {
	if c.PollInterval == 0 {
		c.PollInterval = 20 * time.Millisecond
	}
	if c.Backoff == nil {
		c.Backoff = func(int) time.Duration { return 10 * time.Millisecond }
	}
	return c
}

func newService(t *testing.T, db *gorm.DB, cfg Config) *Service {
	t.Helper()
	s, err := New(db, fastConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func start(t *testing.T, s *Service) {
	t.Helper()
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func jobStatus(t *testing.T, s *Service, id uuid.UUID) Job {
	t.Helper()
	j, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestDB_EnqueueRunsJob(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		s := newService(t, tdb.open(t), Config{})
		org := uuid.New()
		got := make(chan Job, 1)
		s.Register("erp.post_order", func(ctx context.Context, job Job) error {
			got <- job
			return nil
		})
		job, err := s.Enqueue(context.Background(), "erp.post_order", map[string]string{"order": "A-1"}, WithOrg(org))
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != StatusPending || job.OrgID == nil || *job.OrgID != org || job.MaxAttempts != DefaultMaxAttempts {
			t.Fatalf("enqueued job = %+v", job)
		}
		start(t, s)
		run := <-got
		var p map[string]string
		if err := run.Decode(&p); err != nil || p["order"] != "A-1" || run.Attempts != 1 {
			t.Fatalf("handler saw %+v (%v)", run, err)
		}
		waitFor(t, "done", func() bool { return jobStatus(t, s, job.ID).Status == StatusDone })
	})
}

func TestDB_DelayedJobWaits(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		s := newService(t, tdb.open(t), Config{})
		var ran atomic.Int32
		s.Register("k", func(context.Context, Job) error { ran.Add(1); return nil })
		job, _ := s.Enqueue(context.Background(), "k", nil, WithDelay(time.Hour))
		start(t, s)
		time.Sleep(150 * time.Millisecond)
		if ran.Load() != 0 || jobStatus(t, s, job.ID).Status != StatusPending {
			t.Fatal("a delayed job must not run before run_at")
		}
		// Retry on a pending job pulls it forward.
		if _, err := s.Retry(context.Background(), job.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "pulled-forward job", func() bool { return ran.Load() == 1 })
	})
}

func TestDB_Dedupe(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		s := newService(t, tdb.open(t), Config{})
		ctx := context.Background()
		a, err := s.Enqueue(ctx, "meli.sync", map[string]int{"n": 1}, WithDedupeKey("item-1"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Enqueue(ctx, "meli.sync", map[string]int{"n": 2}, WithDedupeKey("item-1"))
		if !errors.Is(err, ErrDuplicate) || b.ID != a.ID {
			t.Fatalf("duplicate enqueue = %v, %v; want ErrDuplicate with the first job", b.ID, err)
		}
		// Same key under another kind is independent.
		if _, err := s.Enqueue(ctx, "erp.post", nil, WithDedupeKey("item-1")); err != nil {
			t.Fatalf("other kind: %v", err)
		}
		// Duplicate inside a transaction must not abort the transaction.
		err = tdb.open(t).Transaction(func(tx *gorm.DB) error {
			if _, err := s.EnqueueTx(tx, "meli.sync", nil, WithDedupeKey("item-1")); !errors.Is(err, ErrDuplicate) {
				return fmt.Errorf("want ErrDuplicate in tx, got %v", err)
			}
			_, err := s.EnqueueTx(tx, "meli.sync", nil, WithDedupeKey("item-2"))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		// Once the first job is done the key is free again.
		s.Register("meli.sync", func(context.Context, Job) error { return nil })
		start(t, s)
		waitFor(t, "first job done", func() bool { return jobStatus(t, s, a.ID).Status == StatusDone })
		c, err := s.Enqueue(ctx, "meli.sync", nil, WithDedupeKey("item-1"))
		if err != nil || c.ID == a.ID {
			t.Fatalf("re-enqueue after done = %v, %v", c.ID, err)
		}
	})
}

func TestDB_EnqueueTxAtomicity(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		db := tdb.open(t)
		s := newService(t, db, Config{})
		if err := db.Exec("CREATE TABLE orders_" + tdb.name + " (id TEXT PRIMARY KEY)").Error; err != nil {
			t.Fatal(err)
		}
		boom := errors.New("payment declined")
		var rolledBack Job
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("INSERT INTO orders_" + tdb.name + " (id) VALUES ('o1')").Error; err != nil {
				return err
			}
			j, err := s.EnqueueTx(tx, "erp.post_order", map[string]string{"id": "o1"})
			if err != nil {
				return err
			}
			rolledBack = j
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("tx err = %v", err)
		}
		if _, err := s.Get(context.Background(), rolledBack.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rolled-back job must not exist, Get err = %v", err)
		}
		if _, total, _ := s.List(context.Background(), Filter{}); total != 0 {
			t.Fatalf("want 0 jobs after rollback, got %d", total)
		}

		var committed Job
		err = db.Transaction(func(tx *gorm.DB) error {
			j, err := s.EnqueueTx(tx, "erp.post_order", map[string]string{"id": "o2"})
			committed = j
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if j := jobStatus(t, s, committed.ID); j.Status != StatusPending {
			t.Fatalf("committed job = %+v", j)
		}
	})
}

func TestDB_RetryBackoffThenSuccess(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		var retries atomic.Int32
		var delays []time.Duration
		var mu sync.Mutex
		s := newService(t, tdb.open(t), Config{
			Hooks: Hooks{OnRetry: func(j Job, err error, next time.Time) {
				retries.Add(1)
				mu.Lock()
				delays = append(delays, time.Until(next))
				mu.Unlock()
			}},
			Backoff: func(attempt int) time.Duration { return time.Duration(attempt) * 30 * time.Millisecond },
		})
		var calls atomic.Int32
		s.Register("flaky", func(ctx context.Context, job Job) error {
			if calls.Add(1) < 3 {
				return fmt.Errorf("upstream 503 (attempt %d)", job.Attempts)
			}
			return nil
		})
		job, _ := s.Enqueue(context.Background(), "flaky", nil)
		start(t, s)
		waitFor(t, "done after retries", func() bool { return jobStatus(t, s, job.ID).Status == StatusDone })
		j := jobStatus(t, s, job.ID)
		if j.Attempts != 3 || calls.Load() != 3 || retries.Load() != 2 || j.LastError != "" {
			t.Fatalf("attempts=%d calls=%d retries=%d last_error=%q", j.Attempts, calls.Load(), retries.Load(), j.LastError)
		}
		mu.Lock()
		defer mu.Unlock()
		if delays[1] <= delays[0] {
			t.Fatalf("backoff should grow: %v", delays)
		}
	})
}

func TestDB_DeadLetterAndRetry(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		var dead atomic.Int32
		s := newService(t, tdb.open(t), Config{MaxAttempts: 3, Hooks: Hooks{OnDead: func(Job, error) { dead.Add(1) }}})
		var fail atomic.Bool
		fail.Store(true)
		var calls atomic.Int32
		s.Register("erp", func(context.Context, Job) error {
			calls.Add(1)
			if fail.Load() {
				return errors.New("ERP rejected: invalid tax id")
			}
			return nil
		})
		job, _ := s.Enqueue(context.Background(), "erp", nil)
		perm, _ := s.Enqueue(context.Background(), "erp", nil, WithMaxAttempts(5))
		s.Register("perm", func(context.Context, Job) error { return Permanent(errors.New("422 unprocessable")) })
		permJob, _ := s.Enqueue(context.Background(), "perm", nil, WithMaxAttempts(5))
		start(t, s)

		waitFor(t, "dead-letter", func() bool { return jobStatus(t, s, job.ID).Status == StatusDead })
		j := jobStatus(t, s, job.ID)
		if j.Attempts != 3 || !strings.Contains(j.LastError, "invalid tax id") {
			t.Fatalf("dead job = %+v", j)
		}
		waitFor(t, "permanent dead", func() bool { return jobStatus(t, s, permJob.ID).Status == StatusDead })
		if pj := jobStatus(t, s, permJob.ID); pj.Attempts != 1 {
			t.Fatalf("Permanent error should dead-letter on attempt 1, got %d", pj.Attempts)
		}
		waitFor(t, "5-attempt job dead", func() bool { return jobStatus(t, s, perm.ID).Status == StatusDead })
		if n := jobStatus(t, s, perm.ID).Attempts; n != 5 {
			t.Fatalf("WithMaxAttempts(5) job used %d attempts", n)
		}
		// Status is committed before OnDead runs, so a poll can see Dead
		// while the hook is still in flight (SQLite, -race).
		waitFor(t, "OnDead x3", func() bool { return dead.Load() == 3 })

		// Done / running jobs cannot be retried; unknown ids are not found.
		if _, err := s.Retry(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("retry unknown = %v", err)
		}
		fail.Store(false)
		r, err := s.Retry(context.Background(), job.ID)
		if err != nil || r.Status != StatusPending || r.Attempts != 0 {
			t.Fatalf("retry dead = %+v, %v", r, err)
		}
		waitFor(t, "retried job done", func() bool { return jobStatus(t, s, job.ID).Status == StatusDone })
		if _, err := s.Retry(context.Background(), job.ID); !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("retry done = %v", err)
		}

		dl, total, err := s.List(context.Background(), Filter{Status: StatusDead})
		if err != nil || total != 2 || len(dl) != 2 {
			t.Fatalf("List(dead) = %d/%d, %v", len(dl), total, err)
		}
	})
}

func TestDB_TwoWorkersNeverRunTheSameJob(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		const jobs = 200
		var runs sync.Map // job id → *atomic.Int32
		var total atomic.Int32
		handler := func(ctx context.Context, job Job) error {
			c, _ := runs.LoadOrStore(job.ID, new(atomic.Int32))
			c.(*atomic.Int32).Add(1)
			total.Add(1)
			time.Sleep(time.Millisecond)
			return nil
		}
		// Two "replicas", each with its own pool (Postgres) and 4 workers.
		a := newService(t, tdb.open(t), Config{Workers: 4, WorkerID: "replica-a"})
		b := newService(t, tdb.open(t), Config{Workers: 4, WorkerID: "replica-b"})
		a.Register("sync", handler)
		b.Register("sync", handler)
		for i := range jobs {
			if _, err := a.Enqueue(context.Background(), "sync", map[string]int{"i": i}); err != nil {
				t.Fatal(err)
			}
		}
		start(t, a)
		start(t, b)
		waitFor(t, "all jobs", func() bool {
			_, n, _ := a.List(context.Background(), Filter{Status: StatusDone, Limit: 1})
			return n == jobs
		})
		if total.Load() != jobs {
			t.Fatalf("handler ran %d times for %d jobs", total.Load(), jobs)
		}
		runs.Range(func(k, v any) bool {
			if n := v.(*atomic.Int32).Load(); n != 1 {
				t.Errorf("job %v ran %d times", k, n)
			}
			return true
		})
	})
}

func TestDB_StuckJobRecoveryAndReap(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		now := func() time.Time { return time.Unix(0, clock.Load()) }
		db := tdb.open(t)
		// "Crashed" replica: claims jobs and never finishes them.
		crashed := newService(t, db, Config{LeaseTimeout: time.Minute, Now: now})
		crashed.Register("erp", func(context.Context, Job) error { return nil })
		recoverable, _ := crashed.Enqueue(context.Background(), "erp", nil)
		lastTry, _ := crashed.Enqueue(context.Background(), "erp", nil, WithMaxAttempts(1))
		claimed, err := crashed.claim(context.Background(), 10, "crashed/1")
		if err != nil || len(claimed) != 2 {
			t.Fatalf("claim = %d, %v", len(claimed), err)
		}
		if again, _ := crashed.claim(context.Background(), 10, "other/1"); len(again) != 0 {
			t.Fatal("a job under a live lease must not be claimed again")
		}

		clock.Add(int64(2 * time.Minute)) // lease expired
		reaped, err := crashed.ReapStuck(context.Background())
		if err != nil || len(reaped) != 1 || reaped[0].ID != lastTry.ID {
			t.Fatalf("reap = %v, %v; want only the job with no attempts left", reaped, err)
		}
		if j := jobStatus(t, crashed, lastTry.ID); j.Status != StatusDead || !strings.Contains(j.LastError, "lease expired") {
			t.Fatalf("reaped job = %+v", j)
		}

		healthy := newService(t, db, Config{LeaseTimeout: time.Minute, Now: now})
		ran := make(chan Job, 1)
		healthy.Register("erp", func(_ context.Context, j Job) error { ran <- j; return nil })
		start(t, healthy)
		j := <-ran
		if j.ID != recoverable.ID || j.Attempts != 2 {
			t.Fatalf("recovered job = %+v; want attempt 2 of %s", j, recoverable.ID)
		}
		waitFor(t, "recovered job done", func() bool { return jobStatus(t, healthy, recoverable.ID).Status == StatusDone })

		// The crashed worker's late result is fenced off by the lease token.
		crashed.finish(claimed[0], "crashed/1", errors.New("late failure"), 0, nil)
		if st := jobStatus(t, healthy, recoverable.ID).Status; st != StatusDone {
			t.Fatalf("stale worker overwrote the job: %s", st)
		}
	})
}

func TestDB_LeaseHeartbeatKeepsLongJob(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		db := tdb.open(t)
		var calls atomic.Int32
		h := func(ctx context.Context, _ Job) error {
			calls.Add(1)
			select {
			case <-time.After(700 * time.Millisecond): // > LeaseTimeout
				return nil
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		a := newService(t, db, Config{LeaseTimeout: 300 * time.Millisecond})
		b := newService(t, db, Config{LeaseTimeout: 300 * time.Millisecond})
		a.Register("slow", h)
		b.Register("slow", h)
		job, _ := a.Enqueue(context.Background(), "slow", nil)
		start(t, a)
		start(t, b)
		waitFor(t, "slow job done", func() bool { return jobStatus(t, a, job.ID).Status == StatusDone })
		if calls.Load() != 1 {
			t.Fatalf("heartbeat should keep the lease; handler ran %d times", calls.Load())
		}
	})
}

func TestDB_ShutdownRequeuesInterruptedJob(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		s := newService(t, tdb.open(t), Config{})
		started := make(chan struct{})
		s.Register("block", func(ctx context.Context, _ Job) error {
			close(started)
			<-ctx.Done()
			return context.Cause(ctx)
		})
		job, _ := s.Enqueue(context.Background(), "block", nil)
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
			t.Fatalf("second Start = %v", err)
		}
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown past grace = %v", err)
		}
		j := jobStatus(t, s, job.ID)
		if j.Status != StatusPending || j.Attempts != 0 {
			t.Fatalf("interrupted job = %s attempts %d; want pending with the attempt given back", j.Status, j.Attempts)
		}
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatalf("second Shutdown = %v", err)
		}
	})
}

func TestDB_StartContextCancelStopsWorkers(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		s := newService(t, tdb.open(t), Config{})
		if err := s.Start(context.Background()); !errors.Is(err, ErrNoHandlers) {
			t.Fatalf("Start without handlers = %v", err)
		}
		var ran atomic.Int32
		s.Register("k", func(context.Context, Job) error { ran.Add(1); return nil })
		ctx, cancel := context.WithCancel(context.Background())
		if err := s.Start(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case <-s.stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not stop after ctx cancel")
		}
		_, _ = s.Enqueue(context.Background(), "k", nil)
		time.Sleep(100 * time.Millisecond)
		if ran.Load() != 0 {
			t.Fatal("stopped service must not run jobs")
		}
	})
}

func TestDB_ListFiltersAndCleanup(t *testing.T) {
	eachDialect(t, func(t *testing.T, tdb testDB) {
		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		s := newService(t, tdb.open(t), Config{RetainDone: time.Hour, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
		ctx := context.Background()
		orgA, orgB := uuid.New(), uuid.New()
		for i := range 3 {
			_, _ = s.Enqueue(ctx, "a", map[string]int{"i": i}, WithOrg(orgA))
		}
		_, _ = s.Enqueue(ctx, "b", nil, WithOrg(orgB))
		_, _ = s.Enqueue(ctx, "b", nil)

		if _, n, _ := s.List(ctx, Filter{OrgID: &orgA}); n != 3 {
			t.Fatalf("org A total = %d", n)
		}
		if page, n, _ := s.List(ctx, Filter{Kind: "b", Limit: 1}); n != 2 || len(page) != 1 {
			t.Fatalf("kind b page = %d/%d", len(page), n)
		}
		if _, n, _ := s.List(ctx, Filter{Status: StatusDead}); n != 0 {
			t.Fatalf("dead = %d", n)
		}

		s.Register("a", func(context.Context, Job) error { return nil })
		start(t, s)
		waitFor(t, "org A done", func() bool {
			_, n, _ := s.List(ctx, Filter{Status: StatusDone})
			return n == 3
		})
		if n, err := s.Cleanup(ctx); err != nil || n != 0 {
			t.Fatalf("cleanup inside retention = %d, %v", n, err)
		}
		clock.Add(int64(2 * time.Hour))
		if n, err := s.Cleanup(ctx); err != nil || n != 3 {
			t.Fatalf("cleanup after retention = %d, %v", n, err)
		}
	})
}
