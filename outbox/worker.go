package outbox

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"strings"
	"time"
)

// errShutdown is the context cause handlers see when Shutdown's deadline
// passes; their jobs are requeued without spending an attempt.
var errShutdown = errors.New("outbox: shutting down")

// ExponentialBackoff returns the default schedule: attempt n waits
// base·2^(n-1) capped at max, with "equal jitter" (a uniformly random
// point in the upper half) so a burst of failures does not retry in
// lockstep against a recovering API.
func ExponentialBackoff(base, max time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		d := base
		for i := 1; i < attempt && d < max; i++ {
			d *= 2
		}
		d = min(d, max)
		half := d / 2
		if half <= 0 {
			return d
		}
		return half + rand.N(half+1)
	}
}

// Start launches the poller, Config.Workers job slots and the maintenance
// loop (stuck-job reaper + retention cleanup), then returns. Cancelling
// ctx begins a graceful shutdown bounded by Config.ShutdownGrace; call
// Shutdown to wait for it explicitly.
func (s *Service) Start(ctx context.Context) error {
	if len(s.kinds()) == 0 {
		return ErrNoHandlers
	}
	if !s.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	pollCtx, stop := context.WithCancel(ctx)
	s.stopPoll = stop
	s.jobCtx, s.cancelJobs = context.WithCancelCause(context.WithoutCancel(ctx))

	s.loops.Add(2)
	go s.pollLoop(pollCtx)
	go s.maintenanceLoop(pollCtx)
	go func() {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownGrace)
			defer cancel()
			_ = s.Shutdown(sctx)
		case <-s.stopped:
		}
	}()
	s.log.Info("outbox workers started", "workers", s.cfg.Workers, "kinds", s.kinds())
	return nil
}

// Shutdown stops claiming new jobs and waits for in-flight ones. When ctx
// expires first, handler contexts are cancelled (cause: shutdown), jobs
// whose handler then fails are put back to pending without spending an
// attempt, and Shutdown returns ctx.Err(). Safe to call more than once and
// before Start.
func (s *Service) Shutdown(ctx context.Context) error {
	if !s.started.Load() {
		return nil
	}
	var err error
	first := false
	s.stopOnce.Do(func() {
		first = true
		s.stopPoll()
		s.loops.Wait()
		done := make(chan struct{})
		go func() { s.jobs.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			s.cancelJobs(errShutdown)
			<-done
			err = ctx.Err()
		}
		s.cancelJobs(errShutdown)
		close(s.stopped)
		s.log.Info("outbox workers stopped")
	})
	if !first {
		select {
		case <-s.stopped:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *Service) pollLoop(ctx context.Context) {
	defer s.loops.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.wake:
		}
		full := s.pollOnce(ctx)
		next := s.cfg.PollInterval
		if full {
			next = 0 // the queue may hold more due jobs: poll again at once
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

// pollOnce claims up to the number of free slots and dispatches them. It
// reports whether it filled every slot it asked for.
func (s *Service) pollOnce(ctx context.Context) bool {
	free := s.cfg.Workers - int(s.inflight.Load())
	if free <= 0 || ctx.Err() != nil {
		return false
	}
	n := min(free, s.cfg.BatchSize)
	token := newToken(s.cfg.WorkerID)
	jobs, err := s.claim(context.WithoutCancel(ctx), n, token)
	if err != nil {
		s.log.Error("outbox: claim failed", "error", err)
		return false
	}
	for _, job := range jobs {
		s.inflight.Add(1)
		s.jobs.Add(1)
		go s.run(job, token)
	}
	return len(jobs) == n && s.cfg.Workers-int(s.inflight.Load()) > 0
}

// claim atomically moves up to n due jobs to running under a fresh lease.
// Due means pending with run_at <= now, or running with an expired lease
// and attempts left (stuck-job recovery). On Postgres the candidate rows
// are locked with FOR UPDATE SKIP LOCKED, so concurrent pollers on any
// number of replicas never claim the same job.
func (s *Service) claim(ctx context.Context, n int, token string) ([]Job, error) {
	now := s.now()
	kinds := s.kinds()
	lease := now.Add(s.cfg.LeaseTimeout)
	candidates := `SELECT id FROM ` + TableName + `
		WHERE kind IN ? AND (
			(status = 'pending' AND run_at <= ?)
			OR (status = 'running' AND locked_until < ? AND attempts < max_attempts))
		ORDER BY run_at, created_at
		LIMIT ?`
	var q string
	if s.pg {
		q = `WITH due AS (` + candidates + ` FOR UPDATE SKIP LOCKED)
			UPDATE ` + TableName + ` AS j SET status = 'running', attempts = j.attempts + 1,
				locked_until = ?, locked_by = ?, updated_at = ?
			FROM due WHERE j.id = due.id
			RETURNING ` + prefixed("j.", jobColumns)
	} else {
		q = `UPDATE ` + TableName + ` SET status = 'running', attempts = attempts + 1,
				locked_until = ?, locked_by = ?, updated_at = ?
			WHERE id IN (` + candidates + `)
			RETURNING ` + jobColumns
	}
	var args []any
	if s.pg {
		args = []any{kinds, now, now, n, lease, token, now}
	} else {
		args = []any{lease, token, now, kinds, now, now, n}
	}
	var rows []jobRow
	if err := s.db.WithContext(ctx).Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	jobs := make([]Job, len(rows))
	for i, r := range rows {
		jobs[i] = r.job()
	}
	return jobs, nil
}

func (s *Service) run(job Job, token string) {
	defer func() {
		s.inflight.Add(-1)
		s.jobs.Done()
		s.nudge()
	}()

	jctx, cancel := context.WithCancelCause(s.jobCtx)
	defer cancel(nil)
	hctx := jctx
	if s.cfg.HandlerTimeout > 0 {
		var c context.CancelFunc
		hctx, c = context.WithTimeout(jctx, s.cfg.HandlerTimeout)
		defer c()
	}

	stopBeat := make(chan struct{})
	beatDone := make(chan struct{})
	go s.heartbeat(job, token, cancel, stopBeat, beatDone)

	if h := s.cfg.Hooks.OnStart; h != nil {
		h(job)
	}
	started := time.Now()
	err := s.invoke(hctx, job)
	took := time.Since(started)
	close(stopBeat)
	<-beatDone

	s.finish(job, token, err, took, context.Cause(jctx))
}

func (s *Service) invoke(ctx context.Context, job Job) (err error) {
	h := s.handler(job.Kind)
	if h == nil {
		return fmt.Errorf("outbox: no handler registered for kind %q", job.Kind)
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("outbox: handler panic", "kind", job.Kind, "job_id", job.ID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("outbox: handler panic: %v", r)
		}
	}()
	return h(ctx, job)
}

// heartbeat renews the lease every LeaseTimeout/3 and cancels the handler
// if another worker took the job over.
func (s *Service) heartbeat(job Job, token string, cancel context.CancelCauseFunc, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(max(s.cfg.LeaseTimeout/3, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			now := s.now()
			res := s.db.Exec(`UPDATE `+TableName+` SET locked_until = ?, updated_at = ?
				WHERE id = ? AND status = 'running' AND locked_by = ?`,
				now.Add(s.cfg.LeaseTimeout), now, job.ID, token)
			if res.Error != nil {
				s.log.Warn("outbox: heartbeat failed", "job_id", job.ID, "error", res.Error)
				continue
			}
			if res.RowsAffected == 0 {
				cancel(ErrLeaseLost)
				return
			}
		}
	}
}

func (s *Service) finish(job Job, token string, herr error, took time.Duration, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := s.db.WithContext(ctx)
	now := s.now()
	log := s.log.With("job_id", job.ID, "kind", job.Kind, "attempt", job.Attempts)

	if errors.Is(cause, ErrLeaseLost) {
		log.Warn("outbox: lease lost; another worker owns the job now")
		return
	}
	const owned = ` WHERE id = ? AND status = 'running' AND locked_by = ?`

	var res error
	var affected int64
	exec := func(q string, args ...any) {
		r := db.Exec(q, args...)
		if r.Error != nil {
			res = r.Error
		}
		affected = r.RowsAffected
	}

	switch {
	case herr == nil:
		exec(`UPDATE `+TableName+` SET status = 'done', last_error = '', locked_until = NULL, locked_by = NULL, updated_at = ?`+owned,
			now, job.ID, token)
		if res == nil && affected == 1 {
			job.Status = StatusDone
			if h := s.cfg.Hooks.OnSuccess; h != nil {
				h(job, took)
			}
		}

	case errors.Is(cause, errShutdown):
		// Interrupted by shutdown: not the job's fault, give the attempt back.
		exec(`UPDATE `+TableName+` SET status = 'pending', attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
				run_at = ?, locked_until = NULL, locked_by = NULL, updated_at = ?`+owned,
			now, now, job.ID, token)
		log.Info("outbox: job interrupted by shutdown; requeued")

	case IsPermanent(herr) || job.Attempts >= job.MaxAttempts:
		exec(`UPDATE `+TableName+` SET status = 'dead', last_error = ?, locked_until = NULL, locked_by = NULL, updated_at = ?`+owned,
			truncate(herr.Error()), now, job.ID, token)
		if res == nil && affected == 1 {
			log.Error("outbox: job dead-lettered", "error", herr)
			job.Status, job.LastError = StatusDead, herr.Error()
			if h := s.cfg.Hooks.OnDead; h != nil {
				h(job, herr)
			}
		}

	default:
		delay, ok := retryDelay(herr)
		if !ok {
			delay = s.cfg.Backoff(job.Attempts)
		}
		next := now.Add(delay)
		exec(`UPDATE `+TableName+` SET status = 'pending', last_error = ?, run_at = ?, locked_until = NULL, locked_by = NULL, updated_at = ?`+owned,
			truncate(herr.Error()), next, now, job.ID, token)
		if res == nil && affected == 1 {
			log.Warn("outbox: job failed; retry scheduled", "error", herr, "next_run_at", next)
			job.Status, job.LastError, job.RunAt = StatusPending, herr.Error(), next
			if h := s.cfg.Hooks.OnRetry; h != nil {
				h(job, herr, next)
			}
		}
	}
	if res != nil {
		log.Error("outbox: could not record job result; the lease will expire and the job re-run", "error", res)
	} else if affected == 0 {
		log.Warn("outbox: job was taken over before its result was recorded")
	}
}

// maintenanceLoop dead-letters stuck jobs that have no attempts left and
// applies the retention policy.
func (s *Service) maintenanceLoop(ctx context.Context) {
	defer s.loops.Done()
	every := min(max(s.cfg.LeaseTimeout/2, time.Second), time.Minute)
	t := time.NewTicker(every)
	defer t.Stop()
	lastCleanup := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.ReapStuck(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("outbox: reap stuck jobs failed", "error", err)
		}
		if time.Since(lastCleanup) >= s.cfg.CleanupInterval {
			lastCleanup = time.Now()
			if n, err := s.Cleanup(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("outbox: cleanup failed", "error", err)
			} else if n > 0 {
				s.log.Info("outbox: cleanup removed jobs", "count", n)
			}
		}
	}
}

// ReapStuck dead-letters running jobs whose lease expired after their last
// allowed attempt (the worker crashed or hung on the final try). Jobs with
// attempts left are simply re-claimed by the poller. Runs automatically
// while the workers are started.
func (s *Service) ReapStuck(ctx context.Context) ([]Job, error) {
	now := s.now()
	const msg = "outbox: lease expired on the final attempt (worker crashed or timed out)"
	var rows []jobRow
	err := s.db.WithContext(ctx).Raw(`UPDATE `+TableName+` SET status = 'dead',
			last_error = CASE WHEN last_error = '' THEN ? ELSE last_error END,
			locked_until = NULL, locked_by = NULL, updated_at = ?
		WHERE status = 'running' AND locked_until < ? AND attempts >= max_attempts
		RETURNING `+jobColumns, msg, now, now).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("outbox: reap: %w", err)
	}
	jobs := make([]Job, len(rows))
	for i, r := range rows {
		jobs[i] = r.job()
		s.log.Error("outbox: stuck job dead-lettered", "job_id", r.ID, "kind", r.Kind)
		if h := s.cfg.Hooks.OnDead; h != nil {
			h(jobs[i], errors.New(msg))
		}
	}
	return jobs, nil
}

// truncate caps an error message at maxErrorLen bytes, keeping it valid
// UTF-8 (Postgres rejects invalid byte sequences in TEXT).
func truncate(s string) string {
	if len(s) > maxErrorLen {
		s = s[:maxErrorLen]
	}
	return strings.ToValidUTF8(s, "")
}

func prefixed(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
