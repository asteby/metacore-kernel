// Package schedule is the kernel-side declarative cron runtime (the v3
// `schedules` block). The host (ops) registers a Schedule per (org, key) at
// install/boot and the Scheduler fires the schedule's `do` handler on the
// parsed interval, dispatching through the same prefix-keyed mechanism the
// stage-machine hooks use (wasm:/webhook:/compiled:).
//
// The Scheduler is idempotent on boot: re-registering the same (org, key)
// replaces the prior ticker rather than spinning a second one, so re-running
// the host's install/boot loop never duplicates fires. Time is injected via the
// Clock interface so tests can drive ticks deterministically without sleeping.
//
// Back-compat: a host that wires no Scheduler (or registers no schedules) gets
// no behaviour at all — an addon without a `schedules` block is unaffected.
package schedule

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Dispatcher routes a resolved `do` handler invocation to its backend. It
// mirrors dynamic.ActionDispatcher (one implementation per wasm|webhook|compiled
// prefix) but is declared locally so the scheduler does not depend on the
// dynamic engine — the host adapts its dispatchers to this narrow surface.
type Dispatcher interface {
	// Dispatch fires the handler named by target (the part of `do` after the
	// prefix) for the given org, with the schedule payload. A non-nil error is
	// logged; it does not stop future fires.
	Dispatch(ctx context.Context, orgID uuid.UUID, target string, payload map[string]any) error
}

// Clock abstracts tickers so tests can drive fires deterministically. The
// production implementation (SystemClock) wraps time.NewTicker.
type Clock interface {
	NewTicker(d time.Duration) Ticker
}

// Ticker is the minimal ticker surface the scheduler needs.
type Ticker interface {
	// Chan delivers a value on each tick.
	Chan() <-chan time.Time
	// Stop releases the ticker's resources.
	Stop()
}

// SystemClock is the production Clock backed by time.NewTicker.
type SystemClock struct{}

// NewTicker satisfies Clock.
func (SystemClock) NewTicker(d time.Duration) Ticker { return &systemTicker{t: time.NewTicker(d)} }

type systemTicker struct{ t *time.Ticker }

func (s *systemTicker) Chan() <-chan time.Time { return s.t.C }
func (s *systemTicker) Stop()                   { s.t.Stop() }

// Schedule is a resolved cron job: fire Do every Every for the org. It mirrors
// the host manifest.ScheduleDef the host builds off the installed manifest.
type Schedule struct {
	// Key identifies the schedule within the org (e.g. "sync_issues").
	Key string
	// Every is the Go duration between fires ("30s", "5m", "1h").
	Every string
	// Do is the handler reference: "wasm:<export>" | "webhook:<key>" |
	// "compiled:<fn>".
	Do string
}

type jobKey struct {
	org uuid.UUID
	key string
}

type job struct {
	every  time.Duration
	do     string
	ticker Ticker
	cancel context.CancelFunc
	done   chan struct{}
}

// Scheduler owns the registered cron jobs and their tickers. Safe for
// concurrent use. Construct it with New.
type Scheduler struct {
	mu          sync.Mutex
	clock       Clock
	dispatchers map[string]Dispatcher // keyed by the do prefix (wasm|webhook|compiled)
	logger      *log.Logger
	jobs        map[jobKey]*job
	started     bool
}

// New constructs a Scheduler. A nil clock defaults to SystemClock; a nil logger
// defaults to log.Default(). dispatchers is keyed by the `do` prefix
// (wasm/webhook/compiled); a fire whose prefix has no dispatcher is logged and
// skipped.
func New(clock Clock, dispatchers map[string]Dispatcher, logger *log.Logger) *Scheduler {
	if clock == nil {
		clock = SystemClock{}
	}
	if logger == nil {
		logger = log.Default()
	}
	if dispatchers == nil {
		dispatchers = map[string]Dispatcher{}
	}
	return &Scheduler{
		clock:       clock,
		dispatchers: dispatchers,
		logger:      logger,
		jobs:        map[jobKey]*job{},
	}
}

// Register adds (or replaces) the schedule for (orgID, schedule.Key). Every must
// parse as a positive Go duration and Do must carry a known dispatch prefix,
// else Register returns an error and changes nothing. Registering an existing
// (org, key) stops the prior ticker first, so a re-run of the host's boot loop
// is idempotent (no duplicate fires). When the Scheduler is already Started the
// new job's ticker starts immediately; otherwise it starts on Start.
func (s *Scheduler) Register(orgID uuid.UUID, sched Schedule) error {
	every, err := time.ParseDuration(sched.Every)
	if err != nil || every <= 0 {
		return fmt.Errorf("schedule: %q.every %q is not a positive Go duration", sched.Key, sched.Every)
	}
	if prefix, _, found := strings.Cut(sched.Do, ":"); !found {
		return fmt.Errorf("schedule: %q.do %q has no dispatch prefix", sched.Key, sched.Do)
	} else if _, ok := s.dispatchers[prefix]; !ok {
		// Not fatal to registration semantics, but a schedule that can never
		// fire is almost certainly a wiring bug — surface it at register time.
		return fmt.Errorf("schedule: %q.do %q has no dispatcher for prefix %q", sched.Key, sched.Do, prefix)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	k := jobKey{org: orgID, key: sched.Key}
	if existing, ok := s.jobs[k]; ok {
		s.stopJob(existing)
	}
	j := &job{every: every, do: sched.Do}
	s.jobs[k] = j
	if s.started {
		s.startJob(k, j)
	}
	return nil
}

// Unregister stops and removes the schedule for (orgID, key). Unknown keys are a
// no-op. Called on uninstall.
func (s *Scheduler) Unregister(orgID uuid.UUID, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := jobKey{org: orgID, key: key}
	if j, ok := s.jobs[k]; ok {
		s.stopJob(j)
		delete(s.jobs, k)
	}
}

// Start begins firing all registered jobs. Idempotent: calling Start when
// already started is a no-op. Jobs registered after Start spin up immediately.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started = true
	for k, j := range s.jobs {
		s.startJob(k, j)
	}
}

// Stop stops every job's ticker and goroutine and marks the Scheduler stopped.
// Registrations survive a Stop, so a later Start resumes them. Idempotent.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return
	}
	for _, j := range s.jobs {
		s.stopJob(j)
	}
	s.started = false
}

// startJob spins the ticker + goroutine for a job. Caller holds s.mu.
func (s *Scheduler) startJob(k jobKey, j *job) {
	if j.ticker != nil {
		return // already running
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.ticker = s.clock.NewTicker(j.every)
	j.done = make(chan struct{})
	go s.run(ctx, k, j)
}

// stopJob stops a job's ticker and waits for its goroutine to exit. Caller holds
// s.mu. Safe to call on an already-stopped job.
func (s *Scheduler) stopJob(j *job) {
	if j.ticker == nil {
		return
	}
	j.cancel()
	j.ticker.Stop()
	<-j.done
	j.ticker = nil
	j.cancel = nil
	j.done = nil
}

// run is the per-job loop: on each tick it fires the job's `do` handler. It
// exits when the job's context is cancelled (Stop/Unregister/replace).
func (s *Scheduler) run(ctx context.Context, k jobKey, j *job) {
	defer close(j.done)
	ch := j.ticker.Chan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			s.fire(ctx, k, j.do)
		}
	}
}

// fire dispatches a job's `do` once. Errors are logged, never propagated — a
// failed fire must not stop the schedule (the cron is a backstop).
func (s *Scheduler) fire(ctx context.Context, k jobKey, do string) {
	prefix, target, found := strings.Cut(do, ":")
	if !found {
		s.logger.Printf("schedule: malformed do %q on %s/%s", do, k.org, k.key)
		return
	}
	// s.dispatchers is immutable after New, so read it without s.mu — taking the
	// lock here would deadlock against stopJob, which holds s.mu while waiting
	// for this goroutine to drain.
	dispatcher, ok := s.dispatchers[prefix]
	if !ok {
		s.logger.Printf("schedule: no dispatcher for %q on %s/%s", do, k.org, k.key)
		return
	}
	payload := map[string]any{
		"org":      k.org.String(),
		"schedule": k.key,
	}
	if err := dispatcher.Dispatch(ctx, k.org, target, payload); err != nil {
		s.logger.Printf("schedule: fire %q on %s/%s failed: %v", do, k.org, k.key, err)
	}
}
