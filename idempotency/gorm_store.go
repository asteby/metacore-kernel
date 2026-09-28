package idempotency

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"
)

// GormStoreTable is the table the durable store owns.
const GormStoreTable = "metacore_idempotency_keys"

// DefaultCleanupInterval is how often GormStore deletes expired rows.
const DefaultCleanupInterval = time.Hour

const (
	rowPending = "pending"
	rowDone    = "done"
)

// GormStoreOptions tunes NewGormStore. The zero value is production-ready.
type GormStoreOptions struct {
	// CleanupInterval is how often a background goroutine deletes expired
	// rows. Zero uses DefaultCleanupInterval; a negative value disables
	// the goroutine (call Cleanup yourself, e.g. from a cron).
	CleanupInterval time.Duration

	// SkipMigrate skips the CREATE TABLE IF NOT EXISTS at construction.
	// Set it when the host owns the schema through its own migrations;
	// Migrate exposes the same DDL.
	SkipMigrate bool

	// Logger receives Get/Put/cleanup failures, which the two-method Store
	// interface cannot return. nil uses slog.Default().
	Logger *slog.Logger

	// Now overrides the clock (tests). nil uses time.Now.
	Now func() time.Time
}

// GormStore is a durable, multi-replica Store backed by a SQL table
// (Postgres in production; SQLite is supported for tests and single-node
// apps). It also implements Locker, so the middleware guarantees that
// concurrent requests with the same key execute the handler once.
//
// Rows are keyed by the middleware's composite hash (namespace + method +
// path + client key), so raw client keys never reach the database.
type GormStore struct {
	db     *gorm.DB
	log    *slog.Logger
	now    func() time.Time
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

var (
	_ Store  = (*GormStore)(nil)
	_ Locker = (*GormStore)(nil)
)

// NewGormStore builds the durable store, creates its table unless
// opts.SkipMigrate, and starts the periodic cleanup of expired keys.
// Call Close on shutdown to stop the cleanup goroutine.
//
//	store, err := idempotency.NewGormStore(db, idempotency.GormStoreOptions{})
//	if err != nil { return err }
//	defer store.Close()
//	api.Post("/orders", idempotency.Middleware(idempotency.Config{Store: store}), h.create)
func NewGormStore(db *gorm.DB, opts GormStoreOptions) (*GormStore, error) {
	if db == nil {
		return nil, errors.New("idempotency: NewGormStore requires a *gorm.DB")
	}
	s := &GormStore{db: db, log: opts.Logger, now: opts.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if !opts.SkipMigrate {
		if err := s.Migrate(context.Background()); err != nil {
			return nil, err
		}
	}
	interval := opts.CleanupInterval
	if interval == 0 {
		interval = DefaultCleanupInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if interval > 0 {
		s.wg.Add(1)
		go s.cleanupLoop(ctx, interval)
	}
	return s, nil
}

// Migrate creates the table and its expiry index if they do not exist.
// On Postgres it takes an advisory lock so replicas booting together do
// not race on the DDL.
func (s *GormStore) Migrate(ctx context.Context) error {
	pg := s.db.Dialector.Name() == "postgres"
	ts, blob := "DATETIME", "BLOB"
	if pg {
		ts, blob = "TIMESTAMPTZ", "BYTEA"
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + GormStoreTable + ` (
			key_hash     TEXT    NOT NULL PRIMARY KEY,
			status       TEXT    NOT NULL,
			status_code  INTEGER NOT NULL DEFAULT 0,
			content_type TEXT    NOT NULL DEFAULT '',
			body         ` + blob + `,
			lock_token   TEXT    NOT NULL DEFAULT '',
			expires_at   ` + ts + ` NOT NULL,
			created_at   ` + ts + ` NOT NULL,
			updated_at   ` + ts + ` NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_` + GormStoreTable + `_expires_at ON ` + GormStoreTable + ` (expires_at)`,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if pg {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", advisoryKey(GormStoreTable)).Error; err != nil {
				return err
			}
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("idempotency: migrate %s: %w", GormStoreTable, err)
	}
	return nil
}

// Get implements Store. Only completed, unexpired responses are returned;
// a pending reservation reads as a miss.
func (s *GormStore) Get(key string) (*Stored, bool) {
	var row storedRow
	res := s.db.Raw(`SELECT status, status_code, content_type, body, expires_at FROM `+GormStoreTable+
		` WHERE key_hash = ? AND status = ? AND expires_at > ?`, key, rowDone, s.utcNow()).Scan(&row)
	if res.Error != nil {
		s.log.Warn("idempotency: get failed", "error", res.Error)
		return nil, false
	}
	if res.RowsAffected == 0 {
		return nil, false
	}
	return row.stored(), true
}

// Put implements Store: it records the completed response, replacing a
// pending reservation for the same key.
func (s *GormStore) Put(key string, value Stored) {
	now := s.utcNow()
	err := s.db.Exec(`INSERT INTO `+GormStoreTable+
		` (key_hash, status, status_code, content_type, body, lock_token, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?)
		ON CONFLICT (key_hash) DO UPDATE SET
			status = excluded.status, status_code = excluded.status_code,
			content_type = excluded.content_type, body = excluded.body,
			lock_token = '', expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		key, rowDone, value.StatusCode, value.ContentType, value.Body, value.ExpiresAt.UTC(), now, now).Error
	if err != nil {
		s.log.Warn("idempotency: put failed", "error", err)
	}
}

// Reserve implements Locker. A single INSERT … ON CONFLICT claims the key
// when it is new or its previous row (pending or done) has expired; the
// unique primary key makes the claim atomic across replicas.
func (s *GormStore) Reserve(ctx context.Context, key string, lease time.Duration) (Reservation, error) {
	if lease <= 0 {
		lease = DefaultLockTimeout
	}
	db := s.db.WithContext(ctx)
	for range 3 {
		now := s.utcNow()
		token := newToken()
		var claimed []string
		err := db.Raw(`INSERT INTO `+GormStoreTable+
			` (key_hash, status, status_code, content_type, body, lock_token, expires_at, created_at, updated_at)
			VALUES (?, ?, 0, '', NULL, ?, ?, ?, ?)
			ON CONFLICT (key_hash) DO UPDATE SET
				status = excluded.status, status_code = 0, content_type = '', body = NULL,
				lock_token = excluded.lock_token, expires_at = excluded.expires_at,
				created_at = excluded.created_at, updated_at = excluded.updated_at
			WHERE `+GormStoreTable+`.expires_at <= ?
			RETURNING key_hash`,
			key, rowPending, token, now.Add(lease), now, now, now).Scan(&claimed).Error
		if err != nil {
			return Reservation{}, fmt.Errorf("idempotency: reserve: %w", err)
		}
		if len(claimed) > 0 {
			return Reservation{Acquired: true, Token: token}, nil
		}

		var row storedRow
		res := db.Raw(`SELECT status, status_code, content_type, body, expires_at FROM `+GormStoreTable+
			` WHERE key_hash = ?`, key).Scan(&row)
		if res.Error != nil {
			return Reservation{}, fmt.Errorf("idempotency: reserve lookup: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			continue // released between the INSERT and the SELECT: try again
		}
		if row.Status == rowDone && row.ExpiresAt.After(now) {
			return Reservation{Replay: row.stored()}, nil
		}
		if row.Status == rowPending && row.ExpiresAt.After(now) {
			return Reservation{}, nil // in flight
		}
		// Expired between the INSERT and the SELECT: try again.
	}
	return Reservation{}, nil
}

// Release implements Locker.
func (s *GormStore) Release(ctx context.Context, key, token string) error {
	err := s.db.WithContext(ctx).Exec(`DELETE FROM `+GormStoreTable+
		` WHERE key_hash = ? AND status = ? AND lock_token = ?`, key, rowPending, token).Error
	if err != nil {
		return fmt.Errorf("idempotency: release: %w", err)
	}
	return nil
}

// Cleanup deletes every expired row and returns how many it removed.
func (s *GormStore) Cleanup(ctx context.Context) (int64, error) {
	res := s.db.WithContext(ctx).Exec(`DELETE FROM `+GormStoreTable+` WHERE expires_at <= ?`, s.utcNow())
	if res.Error != nil {
		return 0, fmt.Errorf("idempotency: cleanup: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// Close stops the cleanup goroutine. It does not close the *gorm.DB.
func (s *GormStore) Close() error {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
	return nil
}

func (s *GormStore) cleanupLoop(ctx context.Context, every time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Cleanup(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("idempotency: cleanup failed", "error", err)
			}
		}
	}
}

func (s *GormStore) utcNow() time.Time { return s.now().UTC() }

type storedRow struct {
	Status      string
	StatusCode  int
	ContentType string
	Body        []byte
	ExpiresAt   time.Time
}

func (r storedRow) stored() *Stored {
	return &Stored{StatusCode: r.StatusCode, Body: r.Body, ContentType: r.ContentType, ExpiresAt: r.ExpiresAt}
}

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// advisoryKey maps a table name to a stable pg_advisory_xact_lock key.
func advisoryKey(name string) int64 {
	h := fnv.New64a()
	h.Write([]byte("metacore:migrate:" + name))
	return int64(h.Sum64())
}
