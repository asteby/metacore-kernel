package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a job.
type Status string

const (
	// StatusPending: waiting for RunAt (first run or a scheduled retry).
	StatusPending Status = "pending"
	// StatusRunning: claimed by a worker that holds its lease.
	StatusRunning Status = "running"
	// StatusDone: the handler returned nil.
	StatusDone Status = "done"
	// StatusDead: dead-lettered — attempts exhausted or a Permanent error.
	// Only Retry moves it again.
	StatusDead Status = "dead"
)

// Valid reports whether s is one of the four known statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusDone, StatusDead:
		return true
	}
	return false
}

// Job is one row of metacore_outbox_jobs as handlers and admin UIs see it.
type Job struct {
	ID          uuid.UUID       `json:"id"`
	OrgID       *uuid.UUID      `json:"org_id,omitempty"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	Status      Status          `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	RunAt       time.Time       `json:"run_at"`
	LastError   string          `json:"last_error,omitempty"`
	DedupeKey   *string         `json:"dedupe_key,omitempty"`
	LockedUntil *time.Time      `json:"locked_until,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Decode unmarshals the job payload into v.
func (j Job) Decode(v any) error {
	if err := json.Unmarshal(j.Payload, v); err != nil {
		return fmt.Errorf("outbox: decode %s payload: %w", j.Kind, err)
	}
	return nil
}

// HandlerFunc processes one job. Returning nil marks it done; an error
// schedules a retry with backoff (or dead-letters it once MaxAttempts is
// reached). Wrap the error with Permanent to dead-letter immediately, or
// with RetryAfter to choose the delay. ctx is cancelled when the lease is
// lost or the service shuts down past its grace period — honour it.
//
// Delivery is at-least-once: a crash after the side effect but before the
// job is marked done re-runs the handler. Make handlers idempotent (e.g.
// send job.ID as the idempotency key to the ERP / marketplace).
type HandlerFunc func(ctx context.Context, job Job) error

// Sentinel errors.
var (
	// ErrDuplicate: a pending or running job with the same kind and dedupe
	// key exists. Enqueue returns it together with the existing job, so
	// most callers treat it as success.
	ErrDuplicate = errors.New("outbox: a job with this dedupe key is already pending or running")
	// ErrNotFound: no job with that id (or not visible to the caller).
	ErrNotFound = errors.New("outbox: job not found")
	// ErrNotRetryable: Retry was called on a running or done job.
	ErrNotRetryable = errors.New("outbox: only pending or dead jobs can be retried")
	// ErrAlreadyStarted: Start was called twice.
	ErrAlreadyStarted = errors.New("outbox: already started")
	// ErrNoHandlers: Start was called before any Register.
	ErrNoHandlers = errors.New("outbox: no handlers registered")
	// ErrLeaseLost is the context cause a handler sees when another worker
	// reclaimed its job (lease expired, e.g. after a long GC pause).
	ErrLeaseLost = errors.New("outbox: job lease lost")
)

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying (bad payload, 4xx from the
// remote API): the job is dead-lettered right away.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

type retryAfterError struct {
	err   error
	delay time.Duration
}

func (e retryAfterError) Error() string { return e.err.Error() }
func (e retryAfterError) Unwrap() error { return e.err }

// RetryAfter asks for the next attempt after delay instead of the backoff
// schedule (e.g. honouring a 429 Retry-After). It still counts as an
// attempt toward MaxAttempts.
func RetryAfter(err error, delay time.Duration) error {
	if err == nil {
		return nil
	}
	return retryAfterError{err: err, delay: delay}
}

func retryDelay(err error) (time.Duration, bool) {
	var r retryAfterError
	if errors.As(err, &r) {
		return r.delay, true
	}
	return 0, false
}

// EnqueueOption customises one Enqueue call.
type EnqueueOption func(*enqueueOptions)

type enqueueOptions struct {
	orgID       *uuid.UUID
	dedupeKey   *string
	runAt       time.Time
	delay       time.Duration
	maxAttempts int
}

// WithOrg scopes the job to an organization (admin UIs filter by it).
func WithOrg(orgID uuid.UUID) EnqueueOption {
	return func(o *enqueueOptions) {
		if orgID != uuid.Nil {
			id := orgID
			o.orgID = &id
		}
	}
}

// WithDedupeKey makes the job unique per (kind, key) while it is pending
// or running. A second Enqueue with the same pair returns the existing job
// and ErrDuplicate. Once the first job is done or dead the key is free.
func WithDedupeKey(key string) EnqueueOption {
	return func(o *enqueueOptions) {
		if key != "" {
			k := key
			o.dedupeKey = &k
		}
	}
}

// WithDelay schedules the first attempt delay from now.
func WithDelay(d time.Duration) EnqueueOption {
	return func(o *enqueueOptions) { o.delay = d }
}

// WithRunAt schedules the first attempt at t (overrides WithDelay).
func WithRunAt(t time.Time) EnqueueOption {
	return func(o *enqueueOptions) { o.runAt = t }
}

// WithMaxAttempts overrides Config.MaxAttempts for this job.
func WithMaxAttempts(n int) EnqueueOption {
	return func(o *enqueueOptions) { o.maxAttempts = n }
}

// Filter narrows List. Zero values mean "any".
type Filter struct {
	OrgID  *uuid.UUID
	Kind   string
	Status Status
	// Limit defaults to 50 and is capped at 500.
	Limit  int
	Offset int
}

// Hooks are optional callbacks for metrics and tracing. Each runs
// synchronously on the worker goroutine; keep them cheap.
type Hooks struct {
	OnEnqueue func(job Job)
	OnStart   func(job Job)
	OnSuccess func(job Job, took time.Duration)
	// OnRetry fires when a failed attempt is rescheduled for next.
	OnRetry func(job Job, err error, next time.Time)
	// OnDead fires when a job is dead-lettered.
	OnDead func(job Job, err error)
}
