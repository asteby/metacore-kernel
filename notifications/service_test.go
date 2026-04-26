package notifications

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	// Using a unique shared-cache URI ensures all connections from the gorm
	// pool see the same tables (sqlite :memory: is per-connection otherwise).
	// _busy_timeout makes SQLite wait for write locks instead of returning
	// "database is locked" instantly — critical under the worker pool's
	// concurrent UPDATEs on slow CI runners.
	dsn := fmt.Sprintf("file:notif_%d?mode=memory&cache=shared&_busy_timeout=5000", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite shared-cache + multi-conn pools deadlock under concurrent writes
	// (the worker pool's UPDATEs vs the producers' INSERTs). Pinning the pool
	// to a single connection serializes access at the driver layer and lets
	// the busy_timeout actually do its job. In-memory DB so the bottleneck is
	// negligible for tests.
	if sqlDB, sqlErr := db.DB(); sqlErr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	// GORM's soft-delete requires deleted_at; AutoMigrate would work too but
	// sqlite dialect + BaseUUIDModel's default:gen_random_uuid() don't mix,
	// so we create the table explicitly.
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS notification_queue_entries (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		source_type TEXT,
		source_id TEXT,
		source_name TEXT,
		event TEXT,
		channel TEXT,
		target TEXT,
		message TEXT,
		context_ref TEXT,
		handler_hint TEXT,
		status TEXT DEFAULT 'pending',
		attempts INTEGER DEFAULT 0,
		max_retries INTEGER DEFAULT 3,
		error TEXT,
		sent_at DATETIME,
		next_retry DATETIME,
		dedup_key TEXT
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func fastConfig(db *gorm.DB) Config {
	return Config{
		DB:              db,
		WorkerCount:     2,
		PollInterval:    20 * time.Millisecond,
		RetryBaseDelay:  5 * time.Millisecond,
		MaxRetries:      2,
		DedupWindow:     500 * time.Millisecond,
		QueueBufferSize: 16,
	}
}

// TestEnqueueAndDeliver verifies the happy path: enqueue → worker picks it up
// → handler delivers → entry marked sent.
func TestEnqueueAndDeliver(t *testing.T) {
	db := setupTestDB(t)
	svc := New(fastConfig(db))

	var delivered atomic.Int32
	svc.Register("test", HandlerFunc(func(ctx context.Context, e *QueueEntry) error {
		delivered.Add(1)
		return nil
	}))
	defer svc.Shutdown()

	res, err := svc.Enqueue(context.Background(), EnqueueRequest{
		OrganizationID: uuid.New(),
		Event:          "hello",
		Channel:        "test",
		Target:         "nobody",
		Message:        "hi",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if res.EntryID == uuid.Nil {
		t.Fatal("expected entry id")
	}

	waitUntil(t, 3*time.Second, func() bool { return delivered.Load() == 1 })

	var entry QueueEntry
	db.First(&entry, "id = ?", res.EntryID)
	if entry.Status != StatusSent {
		t.Fatalf("status = %q, want %q", entry.Status, StatusSent)
	}
	if entry.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", entry.Attempts)
	}
}

// TestDedup within window drops the duplicate.
func TestDedupWithinWindow(t *testing.T) {
	db := setupTestDB(t)
	cfg := fastConfig(db)
	cfg.DedupWindow = time.Hour // force dedup hit
	svc := New(cfg)

	var delivered atomic.Int32
	svc.Register("test", HandlerFunc(func(ctx context.Context, e *QueueEntry) error {
		delivered.Add(1)
		return nil
	}))
	defer svc.Shutdown()

	orgID := uuid.New()
	req := EnqueueRequest{OrganizationID: orgID, Event: "e", Channel: "test", Target: "t", Message: "m"}

	r1, err := svc.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if r1.Deduplicated {
		t.Fatal("first enqueue must not be dedup")
	}
	// let worker pick up the first entry
	waitUntil(t, 2*time.Second, func() bool { return delivered.Load() == 1 })

	r2, err := svc.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if !r2.Deduplicated {
		t.Fatalf("second enqueue should have been deduplicated (entryID=%s)", r2.EntryID)
	}

	var count int64
	db.Model(&QueueEntry{}).Where("organization_id = ?", orgID).Count(&count)
	if count != 1 {
		t.Fatalf("queue count = %d, want 1", count)
	}
}

// TestConcurrentEnqueueCoalesced hammers Enqueue with the same key from many
// goroutines; only one DB row must be created (singleflight guarantee).
func TestConcurrentEnqueueCoalesced(t *testing.T) {
	db := setupTestDB(t)
	cfg := fastConfig(db)
	cfg.DedupWindow = time.Hour
	svc := New(cfg)
	svc.Register("test", HandlerFunc(func(ctx context.Context, e *QueueEntry) error { return nil }))
	defer svc.Shutdown()

	orgID := uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Enqueue(context.Background(), EnqueueRequest{
				OrganizationID: orgID,
				Event:          "race",
				Channel:        "test",
				Target:         "x",
				Message:        "same",
			})
		}()
	}
	wg.Wait()

	var count int64
	db.Model(&QueueEntry{}).Where("organization_id = ?", orgID).Count(&count)
	if count != 1 {
		t.Fatalf("concurrent enqueue should yield 1 row, got %d", count)
	}
}

// TestRetryUntilMax: handler returns an error → entry bounces until MaxRetries
// then lands in failed.
func TestRetryUntilMax(t *testing.T) {
	db := setupTestDB(t)
	cfg := fastConfig(db)
	cfg.MaxRetries = 2
	svc := New(cfg)

	var attempts atomic.Int32
	svc.Register("flaky", HandlerFunc(func(ctx context.Context, e *QueueEntry) error {
		attempts.Add(1)
		return errors.New("boom")
	}))
	defer svc.Shutdown()

	r, err := svc.Enqueue(context.Background(), EnqueueRequest{
		OrganizationID: uuid.New(),
		Event:          "flaky",
		Channel:        "flaky",
		Message:        "m",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		var entry QueueEntry
		db.First(&entry, "id = ?", r.EntryID)
		return entry.Status == StatusFailed
	})
	if attempts.Load() < 2 {
		t.Fatalf("handler invoked %d times, want >= 2", attempts.Load())
	}
}

// TestPermanentFailureNoRetry: handler returns ErrPermanent → no retry.
func TestPermanentFailureNoRetry(t *testing.T) {
	db := setupTestDB(t)
	cfg := fastConfig(db)
	cfg.MaxRetries = 5
	svc := New(cfg)

	var attempts atomic.Int32
	svc.Register("bad", HandlerFunc(func(ctx context.Context, e *QueueEntry) error {
		attempts.Add(1)
		return fmt.Errorf("invalid target: %w", ErrPermanent)
	}))
	defer svc.Shutdown()

	r, err := svc.Enqueue(context.Background(), EnqueueRequest{
		OrganizationID: uuid.New(),
		Channel:        "bad",
		Message:        "m",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitUntil(t, 2*time.Second, func() bool {
		var entry QueueEntry
		db.First(&entry, "id = ?", r.EntryID)
		return entry.Status == StatusFailed
	})
	if got := attempts.Load(); got != 1 {
		t.Fatalf("permanent error should skip retries, handler called %d times", got)
	}
}

// TestNoHandlerFailsImmediately: enqueue to an unregistered channel fails fast.
func TestNoHandlerFailsImmediately(t *testing.T) {
	db := setupTestDB(t)
	svc := New(fastConfig(db))
	defer svc.Shutdown()

	r, err := svc.Enqueue(context.Background(), EnqueueRequest{
		OrganizationID: uuid.New(),
		Channel:        "ghost",
		Message:        "m",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		var entry QueueEntry
		db.First(&entry, "id = ?", r.EntryID)
		return entry.Status == StatusFailed
	})
}

// TestBuildDedupKeyStable: same input → same key.
func TestBuildDedupKeyStable(t *testing.T) {
	k1 := BuildDedupKey("e", "c", "t", "m")
	k2 := BuildDedupKey("e", "c", "t", "m")
	if k1 != k2 {
		t.Fatalf("non-deterministic: %s vs %s", k1, k2)
	}
	if BuildDedupKey("e", "c", "t", "m") == BuildDedupKey("e", "c", "t", "n") {
		t.Fatal("messages must differ in hash")
	}
	if len(k1) != 32 {
		t.Fatalf("expected 32 hex chars, got %d", len(k1))
	}
}

// TestEnqueueRequiresChannelAndMessage covers the cheap input validations.
func TestEnqueueRequiresChannelAndMessage(t *testing.T) {
	db := setupTestDB(t)
	svc := New(fastConfig(db))
	defer svc.Shutdown()

	if _, err := svc.Enqueue(context.Background(), EnqueueRequest{Channel: "x"}); err == nil {
		t.Fatal("missing message: want error")
	}
	if _, err := svc.Enqueue(context.Background(), EnqueueRequest{Message: "x"}); err == nil {
		t.Fatal("missing channel: want error")
	}
}

// waitUntil polls cond() until true or timeout.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("wait timeout after %s", timeout)
}
