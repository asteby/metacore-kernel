package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Defaults applied by New to zero Config fields.
const (
	DefaultWorkers         = 4
	DefaultPollInterval    = time.Second
	DefaultLeaseTimeout    = 5 * time.Minute
	DefaultMaxAttempts     = 10
	DefaultBackoffBase     = 10 * time.Second
	DefaultBackoffMax      = time.Hour
	DefaultRetainDone      = 7 * 24 * time.Hour
	DefaultCleanupInterval = time.Hour
	DefaultShutdownGrace   = 30 * time.Second
)

// maxErrorLen caps last_error so a huge upstream response body cannot bloat
// the row.
const maxErrorLen = 4096

// Config tunes the Service. The zero value is production-ready.
type Config struct {
	// Workers is how many jobs this process runs concurrently.
	Workers int
	// PollInterval is how often an idle process looks for due jobs.
	// Enqueue (outside a transaction) wakes local workers immediately.
	PollInterval time.Duration
	// BatchSize caps how many jobs one poll claims. Defaults to Workers.
	BatchSize int
	// LeaseTimeout is the visibility timeout: a running job whose worker
	// stopped heartbeating for this long is considered stuck and is
	// claimed again (or dead-lettered if it has no attempts left). Running
	// jobs renew their lease every LeaseTimeout/3.
	LeaseTimeout time.Duration
	// HandlerTimeout bounds a single attempt. Zero means no limit beyond
	// the lease heartbeat and shutdown.
	HandlerTimeout time.Duration
	// MaxAttempts is the default attempt budget per job (WithMaxAttempts
	// overrides it per job).
	MaxAttempts int
	// BackoffBase and BackoffMax shape the default exponential backoff
	// with jitter: attempt n waits ~BackoffBase·2^(n-1), capped at
	// BackoffMax.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// Backoff replaces the default schedule. attempt is 1 after the first
	// failure.
	Backoff func(attempt int) time.Duration
	// RetainDone is how long done jobs are kept before cleanup deletes
	// them. Negative keeps them forever.
	RetainDone time.Duration
	// RetainDead is how long dead jobs are kept. Zero or negative keeps
	// them forever (the default) so operators can inspect and Retry them.
	RetainDead time.Duration
	// CleanupInterval is how often retention cleanup runs.
	CleanupInterval time.Duration
	// ShutdownGrace is how long in-flight jobs may run after the context
	// passed to Start is cancelled, before their contexts are cancelled.
	ShutdownGrace time.Duration
	// SkipMigrate skips the CREATE TABLE IF NOT EXISTS in New.
	SkipMigrate bool
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Hooks receive lifecycle callbacks for metrics.
	Hooks Hooks
	// WorkerID labels this process in locked_by. Defaults to hostname:pid.
	WorkerID string
	// Now overrides the clock (tests). Timestamps are compared by value
	// across replicas, so keep hosts NTP-synced.
	Now func() time.Time
}

// Service is the durable job queue. Construct with New, Register handlers,
// then Start the workers. Enqueue works without Start (producer-only
// processes).
type Service struct {
	db  *gorm.DB
	cfg Config
	pg  bool
	log *slog.Logger

	mu       sync.RWMutex
	handlers map[string]HandlerFunc

	started    atomic.Bool
	inflight   atomic.Int32
	wake       chan struct{}
	stopPoll   context.CancelFunc
	jobCtx     context.Context
	cancelJobs context.CancelCauseFunc
	loops      sync.WaitGroup
	jobs       sync.WaitGroup
	stopOnce   sync.Once
	stopped    chan struct{}
}

// New builds the Service and (unless cfg.SkipMigrate) creates its table.
// Postgres is the production target; SQLite is supported for tests and
// single-node tools (claims are serialised by SQLite's writer lock).
func New(db *gorm.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("outbox: New requires a *gorm.DB")
	}
	switch db.Dialector.Name() {
	case "postgres", "sqlite":
	default:
		return nil, fmt.Errorf("outbox: unsupported dialect %q (postgres or sqlite)", db.Dialector.Name())
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = cfg.Workers
	}
	if cfg.LeaseTimeout <= 0 {
		cfg.LeaseTimeout = DefaultLeaseTimeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = DefaultBackoffBase
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = DefaultBackoffMax
	}
	if cfg.Backoff == nil {
		cfg.Backoff = ExponentialBackoff(cfg.BackoffBase, cfg.BackoffMax)
	}
	if cfg.RetainDone == 0 {
		cfg.RetainDone = DefaultRetainDone
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = DefaultCleanupInterval
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = DefaultShutdownGrace
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.WorkerID == "" {
		host, _ := os.Hostname()
		cfg.WorkerID = fmt.Sprintf("%s:%d", host, os.Getpid())
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{
		db:       db,
		cfg:      cfg,
		pg:       isPostgres(db),
		log:      cfg.Logger.With("component", "outbox"),
		handlers: map[string]HandlerFunc{},
		wake:     make(chan struct{}, 1),
		stopped:  make(chan struct{}),
	}
	if !cfg.SkipMigrate {
		if err := Migrate(context.Background(), db); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Register binds a handler to a job kind (e.g. "erp.post_order"). Workers
// only claim kinds registered in their process, so several services can
// share the table. Registering the same kind twice panics.
func (s *Service) Register(kind string, h HandlerFunc) {
	if kind == "" || h == nil {
		panic("outbox: Register needs a kind and a handler")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.handlers[kind]; dup {
		panic("outbox: handler already registered for kind " + kind)
	}
	s.handlers[kind] = h
}

// Enqueue stores a job. payload is JSON-encoded ([]byte and
// json.RawMessage are stored verbatim and must be valid JSON). With
// WithDedupeKey, an existing live job is returned together with
// ErrDuplicate.
func (s *Service) Enqueue(ctx context.Context, kind string, payload any, opts ...EnqueueOption) (Job, error) {
	job, err := s.enqueue(s.db.WithContext(ctx), kind, payload, opts)
	if err == nil {
		s.nudge()
	}
	return job, err
}

// EnqueueTx stores a job inside the caller's transaction, so it commits or
// rolls back atomically with the business rows (the transactional outbox
// pattern). Workers see it once tx commits.
//
//	err := db.Transaction(func(tx *gorm.DB) error {
//	    if err := tx.Create(&order).Error; err != nil {
//	        return err
//	    }
//	    _, err := jobs.EnqueueTx(tx, "erp.post_order", OrderRef{ID: order.ID},
//	        outbox.WithOrg(order.OrgID), outbox.WithDedupeKey(order.ID.String()))
//	    return err
//	})
func (s *Service) EnqueueTx(tx *gorm.DB, kind string, payload any, opts ...EnqueueOption) (Job, error) {
	if tx == nil {
		return Job{}, errors.New("outbox: EnqueueTx requires a transaction")
	}
	return s.enqueue(tx, kind, payload, opts)
}

func (s *Service) enqueue(db *gorm.DB, kind string, payload any, opts []EnqueueOption) (Job, error) {
	if kind == "" {
		return Job{}, errors.New("outbox: job kind is required")
	}
	raw, err := encodePayload(payload)
	if err != nil {
		return Job{}, err
	}
	o := enqueueOptions{maxAttempts: s.cfg.MaxAttempts}
	for _, opt := range opts {
		opt(&o)
	}
	if o.maxAttempts <= 0 {
		o.maxAttempts = s.cfg.MaxAttempts
	}
	now := s.now()
	runAt := now.Add(o.delay)
	if !o.runAt.IsZero() {
		runAt = o.runAt.UTC()
	}

	payloadExpr := "?"
	if s.pg {
		payloadExpr = "CAST(? AS jsonb)"
	}
	insert := `INSERT INTO ` + TableName +
		` (id, org_id, kind, payload, status, attempts, max_attempts, run_at, last_error, dedupe_key, created_at, updated_at)
		VALUES (?, ?, ?, ` + payloadExpr + `, ?, 0, ?, ?, '', ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING ` + jobColumns

	for range 3 {
		var rows []jobRow
		err := db.Raw(insert, uuid.New(), o.orgID, kind, string(raw), StatusPending,
			o.maxAttempts, runAt, o.dedupeKey, now, now).Scan(&rows).Error
		if err != nil {
			return Job{}, fmt.Errorf("outbox: enqueue %s: %w", kind, err)
		}
		if len(rows) == 1 {
			job := rows[0].job()
			if h := s.cfg.Hooks.OnEnqueue; h != nil {
				h(job)
			}
			return job, nil
		}
		if o.dedupeKey == nil {
			return Job{}, fmt.Errorf("outbox: enqueue %s: insert returned no row", kind)
		}
		var existing []jobRow
		err = db.Raw(`SELECT `+jobColumns+` FROM `+TableName+
			` WHERE kind = ? AND dedupe_key = ? AND status IN (?, ?) LIMIT 1`,
			kind, *o.dedupeKey, StatusPending, StatusRunning).Scan(&existing).Error
		if err != nil {
			return Job{}, fmt.Errorf("outbox: enqueue %s: dedupe lookup: %w", kind, err)
		}
		if len(existing) == 1 {
			return existing[0].job(), ErrDuplicate
		}
		// The conflicting job finished between INSERT and SELECT: retry.
	}
	return Job{}, fmt.Errorf("outbox: enqueue %s: dedupe key %q kept conflicting", kind, *o.dedupeKey)
}

// Get returns one job.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Job, error) {
	var rows []jobRow
	if err := s.db.WithContext(ctx).Raw(`SELECT `+jobColumns+` FROM `+TableName+` WHERE id = ?`, id).
		Scan(&rows).Error; err != nil {
		return Job{}, fmt.Errorf("outbox: get: %w", err)
	}
	if len(rows) == 0 {
		return Job{}, ErrNotFound
	}
	return rows[0].job(), nil
}

// List returns jobs matching f, newest first, plus the total count of
// matching jobs (for pagination).
func (s *Service) List(ctx context.Context, f Filter) ([]Job, int64, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := s.db.WithContext(ctx).Table(TableName)
	if f.OrgID != nil {
		q = q.Where("org_id = ?", *f.OrgID)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("outbox: list count: %w", err)
	}
	var rows []jobRow
	if err := q.Select(jobColumns).Order("created_at DESC, id").Limit(limit).Offset(max(f.Offset, 0)).
		Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("outbox: list: %w", err)
	}
	jobs := make([]Job, len(rows))
	for i, r := range rows {
		jobs[i] = r.job()
	}
	return jobs, total, nil
}

// Retry makes a dead job runnable again with a fresh attempt budget, or
// pulls a pending job's next attempt forward to now. Running and done
// jobs return ErrNotRetryable; a dead job whose dedupe key is taken by a
// newer live job returns ErrDuplicate.
func (s *Service) Retry(ctx context.Context, id uuid.UUID) (Job, error) {
	now := s.now()
	var rows []jobRow
	err := s.db.WithContext(ctx).Raw(`UPDATE `+TableName+` SET
			status = ?, attempts = CASE WHEN status = ? THEN 0 ELSE attempts END,
			run_at = ?, locked_until = NULL, locked_by = NULL, updated_at = ?
		WHERE id = ? AND status IN (?, ?)
		RETURNING `+jobColumns,
		StatusPending, StatusDead, now, now, id, StatusPending, StatusDead).Scan(&rows).Error
	if err != nil {
		if isUniqueViolation(err) {
			return Job{}, ErrDuplicate
		}
		return Job{}, fmt.Errorf("outbox: retry: %w", err)
	}
	if len(rows) == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return Job{}, err
		}
		return Job{}, ErrNotRetryable
	}
	s.nudge()
	return rows[0].job(), nil
}

// Cleanup applies the retention policy now (it also runs every
// CleanupInterval while the workers are started) and returns how many
// jobs it deleted.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	now := s.now()
	var total int64
	del := func(status Status, keep time.Duration) error {
		if keep <= 0 {
			return nil
		}
		res := s.db.WithContext(ctx).Exec(`DELETE FROM `+TableName+` WHERE status = ? AND updated_at < ?`,
			status, now.Add(-keep))
		total += res.RowsAffected
		return res.Error
	}
	if err := del(StatusDone, s.cfg.RetainDone); err != nil {
		return total, fmt.Errorf("outbox: cleanup done: %w", err)
	}
	if err := del(StatusDead, s.cfg.RetainDead); err != nil {
		return total, fmt.Errorf("outbox: cleanup dead: %w", err)
	}
	return total, nil
}

func (s *Service) handler(kind string) HandlerFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handlers[kind]
}

func (s *Service) kinds() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.handlers))
	for k := range s.handlers {
		out = append(out, k)
	}
	return out
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

func (s *Service) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func encodePayload(payload any) ([]byte, error) {
	switch p := payload.(type) {
	case json.RawMessage:
		if !json.Valid(p) {
			return nil, errors.New("outbox: payload is not valid JSON")
		}
		return p, nil
	case []byte:
		if !json.Valid(p) {
			return nil, errors.New("outbox: payload is not valid JSON")
		}
		return p, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("outbox: encode payload: %w", err)
	}
	return raw, nil
}

func isUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "23505") || strings.Contains(msg, "UNIQUE constraint failed")
}

func newToken(workerID string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return workerID + "/" + hex.EncodeToString(b[:])
}

const jobColumns = `id, org_id, kind, payload, status, attempts, max_attempts, run_at, last_error, dedupe_key, locked_until, created_at, updated_at`

type jobRow struct {
	ID          uuid.UUID
	OrgID       *uuid.UUID
	Kind        string
	Payload     []byte
	Status      string
	Attempts    int
	MaxAttempts int
	RunAt       time.Time
	LastError   string
	DedupeKey   *string
	LockedUntil *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (r jobRow) job() Job {
	return Job{
		ID:          r.ID,
		OrgID:       r.OrgID,
		Kind:        r.Kind,
		Payload:     json.RawMessage(r.Payload),
		Status:      Status(r.Status),
		Attempts:    r.Attempts,
		MaxAttempts: r.MaxAttempts,
		RunAt:       r.RunAt,
		LastError:   r.LastError,
		DedupeKey:   r.DedupeKey,
		LockedUntil: r.LockedUntil,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}
