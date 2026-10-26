package dispatch

// Durable delivery queue.
//
// The ledger used to be "a record + a goroutine": enqueue INSERTed the row and
// spawned a goroutine that retried IN MEMORY. A restart in the middle of the
// retries left the row `pending` forever, because nothing ever read it again.
//
// Now the row IS the work item (the same engine shape as package outbox):
//
//   - it carries the payload, `next_attempt_at` and a lease (`lease_until`,
//     `lease_token`);
//   - a worker CLAIMS due rows with one UPDATE over `SELECT … FOR UPDATE SKIP
//     LOCKED` (Postgres), so any number of workers on any number of replicas
//     never claim the same delivery twice;
//   - the claim bumps `attempts` and takes a lease, renewed by a heartbeat
//     while the handler runs. A process that dies mid-attempt just lets the
//     lease expire; the next sweep claims the row again;
//   - every result write is FENCED by the lease token, so a worker that lost
//     its lease cannot overwrite the new owner;
//   - a failed attempt writes `next_attempt_at = now + backoff` and releases the
//     lease. There is no retry loop in memory: an in-process timer only wakes
//     the sweeper early, and the sweeper also runs on boot and on a ticker.
//
// Delivery is at-least-once (a crash after the side effect and before the
// "delivered" write re-runs the handler), and exactly-once per delivery_id on
// the ledger side. Handlers must stay idempotent against external effects.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultPollInterval   = 5 * time.Second
	defaultLease          = 2 * time.Minute
	defaultAttemptTimeout = 5 * time.Minute

	claimBatch           = 32
	maxPollerConcurrency = 64
)

var errLeaseLost = errors.New("dispatch: delivery lease lost")

func (d *Dispatcher) tbl() *gorm.DB {
	return d.db.Session(&gorm.Session{NewDB: true}).Table(d.opts.tableName)
}

func (d *Dispatcher) isPostgres() bool { return d.db.Dialector.Name() == "postgres" }

func newClaimToken(worker string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return worker + "/" + hex.EncodeToString(b[:])
}

// subscriptionOf rebuilds the Subscription identity stored on a ledger row.
func subscriptionOf(r Delivery) Subscription {
	return Subscription{AddonKey: r.AddonKey, Event: r.SubEvent, HandlerType: r.HandlerType, Function: r.Export}
}

// claim atomically takes up to n due deliveries under a fresh lease and bumps
// their attempt counter. Due = pending, payload present, next_attempt_at
// reached, and no live lease (never leased, or the lease expired because its
// holder died). Rows that already spent every attempt are NOT claimed: the
// reaper dead-letters them instead, so a handler that kills the process cannot
// loop forever.
func (d *Dispatcher) claim(ctx context.Context, n int) ([]Delivery, error) {
	now := time.Now().UTC()
	lease := now.Add(d.opts.lease)
	token := newClaimToken(d.workerID)
	t := d.opts.tableName
	candidates := `SELECT id FROM ` + t + `
		WHERE status = 'pending' AND payload <> '' AND next_attempt_at <= ?
		  AND (lease_until IS NULL OR lease_until < ?) AND attempts < ?
		ORDER BY next_attempt_at, created_at
		LIMIT ?`
	var q string
	var args []any
	if d.isPostgres() {
		q = `WITH due AS (` + candidates + ` FOR UPDATE SKIP LOCKED)
			UPDATE ` + t + ` AS j SET attempts = j.attempts + 1, lease_until = ?, lease_token = ?, updated_at = ?
			FROM due WHERE j.id = due.id
			RETURNING j.*`
		args = []any{now, now, d.opts.maxAttempts, n, lease, token, now}
	} else {
		q = `UPDATE ` + t + ` SET attempts = attempts + 1, lease_until = ?, lease_token = ?, updated_at = ?
			WHERE id IN (` + candidates + `)
			RETURNING *`
		args = []any{lease, token, now, now, now, d.opts.maxAttempts, n}
	}
	var rows []Delivery
	if err := d.db.Session(&gorm.Session{NewDB: true}).WithContext(ctx).Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// pollLoop is the sweeper: once at boot (resume what the previous process
// left), then on a ticker. Early wake-ups come from armWake.
func (d *Dispatcher) pollLoop() {
	d.sweep()
	t := time.NewTicker(d.opts.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-d.baseCtx.Done():
			return
		case <-t.C:
			d.sweep()
		}
	}
}

// sweep claims and runs every due delivery (bounded concurrency).
func (d *Dispatcher) sweep() {
	if d.baseCtx.Err() != nil {
		return
	}
	d.inflight.Add(1)
	defer d.inflight.Add(-1)
	d.reapExhausted()
	for d.baseCtx.Err() == nil {
		free := maxPollerConcurrency - int(d.running.Load())
		if free <= 0 {
			return
		}
		n := free
		if n > claimBatch {
			n = claimBatch
		}
		rows, err := d.claim(d.baseCtx, n)
		if err != nil {
			if d.baseCtx.Err() == nil {
				d.logger.Warn("dispatch.claim_error", slog.String("err", err.Error()))
			}
			return
		}
		for _, r := range rows {
			d.running.Add(1)
			d.inflight.Add(1)
			go func(r Delivery) {
				defer d.inflight.Add(-1)
				defer d.running.Add(-1)
				d.runClaimed(r, nil)
			}(r)
		}
		if len(rows) < n {
			return
		}
	}
}

// armWake runs a sweep after delay so a short retry does not wait for the
// next tick. It is only a latency optimisation: the durable truth is the row's
// next_attempt_at, and a lost timer (restart) is recovered by the boot sweep.
func (d *Dispatcher) armWake(delay time.Duration) {
	if delay > d.opts.pollInterval || d.baseCtx.Err() != nil {
		return
	}
	d.inflight.Add(1)
	time.AfterFunc(delay, func() {
		defer d.inflight.Add(-1)
		d.sweep()
	})
}

// reapExhausted dead-letters deliveries whose lease expired after they had
// already spent every attempt (the process died inside the last one).
func (d *Dispatcher) reapExhausted() {
	now := time.Now().UTC()
	var rows []Delivery
	if err := d.tbl().WithContext(d.baseCtx).
		Where("status = ? AND payload <> '' AND lease_until IS NOT NULL AND lease_until < ? AND attempts >= ?",
			StatusPending, now, d.opts.maxAttempts).
		Limit(50).Find(&rows).Error; err != nil {
		return
	}
	for _, r := range rows {
		msg := fmt.Sprintf("exhausted retries: lease expired during attempt %d (the process handling it died or hung)", r.Attempts)
		res := d.tbl().Where("delivery_id = ? AND lease_token = ? AND status = ?", r.DeliveryID, r.LeaseToken, StatusPending).
			Updates(map[string]any{"status": StatusDead, "last_error": truncErr(msg), "lease_until": nil, "lease_token": ""})
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		sub := subscriptionOf(r)
		d.logger.Error("dispatch.delivery_dead", slog.String("addon", r.AddonKey), slog.String("event", r.Event), slog.String("err", msg))
		d.notify(r.DeliveryID, r.Event, sub, StatusDead, r.Attempts, msg)
		d.emitDeadLetter(r.OrganizationID, r.DeliveryID, r.Event, sub, msg)
	}
}

// resolveSub finds the live subscription a stored row was created for.
func (d *Dispatcher) resolveSub(ctx context.Context, r Delivery) (*Subscription, error) {
	subs, err := d.provider.SubscriptionsForOrg(ctx, r.OrganizationID)
	if err != nil {
		return nil, err
	}
	for i := range subs {
		s := subs[i]
		if s.AddonKey == r.AddonKey && s.Event == r.SubEvent &&
			strings.EqualFold(s.HandlerType, r.HandlerType) && s.Function == r.Export {
			return &s, nil
		}
	}
	return nil, nil
}

// startHeartbeat renews the lease while the handler runs. If the row is no
// longer ours it cancels the attempt (cause errLeaseLost).
func (d *Dispatcher) startHeartbeat(ctx context.Context, r Delivery, cancel context.CancelCauseFunc) (stop func()) {
	every := d.opts.lease / 3
	if every <= 0 {
		every = time.Millisecond
	}
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				// Attempt over (shutdown, timeout): stop renewing, so a
				// handler that ignores its ctx cannot pin the delivery
				// forever — the lease runs out and another worker resumes it.
				return
			case <-t.C:
				now := time.Now().UTC()
				res := d.tbl().Where("delivery_id = ? AND lease_token = ? AND status = ?", r.DeliveryID, r.LeaseToken, StatusPending).
					Updates(map[string]any{"lease_until": now.Add(d.opts.lease), "updated_at": now})
				if res.Error == nil && res.RowsAffected == 0 {
					cancel(errLeaseLost)
					return
				}
			}
		}
	}()
	return func() { close(quit); <-done }
}

// runClaimed runs ONE attempt of a delivery this process holds the lease on.
// sub is non-nil on the enqueue fast path; nil means "resolve it again" (a row
// claimed by the sweeper, possibly after a restart).
func (d *Dispatcher) runClaimed(row Delivery, sub *Subscription) {
	ctx, cancelT := context.WithTimeout(d.baseCtx, d.opts.attemptTimeout)
	defer cancelT()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopHB := d.startHeartbeat(ctx, row, cancel)
	defer stopHB()

	// Re-attach the originating EVENT's identity: host imports the handler
	// calls (data_mutate above all) stamp their own canonical events from this
	// ctx, so without it every downstream mutation surfaced as an anonymous
	// actor in the audit trail. EventID becomes the causation of whatever the
	// handler publishes.
	ctx = dynamic.WithActorID(ctx, row.ActorID)
	ctx = dynamic.WithBranchID(ctx, row.BranchID)
	if row.Emitter != "" {
		ctx = events.WithEmitter(ctx, row.Emitter)
	}
	if row.CorrelationID != "" {
		ctx = dynamic.WithCorrelationID(ctx, row.CorrelationID)
	}
	ctx = dynamic.WithCausationID(ctx, row.EventID)

	if sub == nil {
		s, err := d.resolveSub(ctx, row)
		if err != nil {
			d.logger.Warn("dispatch.provider_error", slog.String("org", row.OrganizationID.String()),
				slog.String("event", row.Event), slog.String("err", err.Error()))
			delay := d.backoffFor(maxInt(row.Attempts+1, 2))
			if delay < time.Second {
				delay = time.Second // a provider outage must not turn into a hot loop
			}
			d.reschedule(row, delay, "provider: "+err.Error(), true, row.NotReadySince)
			return
		}
		if s == nil {
			d.finishCancelled(row, "subscription no longer installed or enabled")
			return
		}
		sub = s
	}

	// Capability gate FIRST — an addon that lacks event:subscribe never has
	// its handler invoked, and the denial is a terminal (dead) outcome, not a
	// retryable error (retrying a permission denial is pointless).
	if d.opts.capability != nil {
		if err := d.opts.capability.CanDeliver(sub.AddonKey, row.Event); err != nil {
			msg := fmt.Sprintf("capability denied: %v", err)
			if d.finishDead(row, msg) {
				d.logger.Warn("dispatch.capability_denied", slog.String("addon", sub.AddonKey), slog.String("event", row.Event))
				d.notify(row.DeliveryID, row.Event, *sub, StatusDead, 0, msg)
			}
			return
		}
	}

	attempt := row.Attempts // the claim already counted this one
	payload := withGeneration([]byte(row.Payload), row.Generation)
	err := d.invoke(ctx, row.OrganizationID, payload, *sub)
	if err == nil {
		if d.finishDelivered(row, attempt) {
			d.logger.Debug("dispatch.delivered",
				slog.String("addon", sub.AddonKey), slog.String("event", row.Event),
				slog.String("function", sub.Function), slog.Int("attempt", attempt))
			d.notify(row.DeliveryID, row.Event, *sub, StatusDelivered, attempt, "")
		}
		return
	}
	if errors.Is(context.Cause(ctx), errLeaseLost) {
		d.logger.Warn("dispatch.lease_lost", slog.String("addon", sub.AddonKey), slog.String("event", row.Event))
		return
	}
	// Shutdown: the attempt was interrupted, not failed. Hand the delivery back
	// without spending the attempt so the next process runs it in full.
	if d.baseCtx.Err() != nil {
		d.release(row)
		return
	}

	// The subscriber is not up yet (its module is still loading after a
	// restart): wait for it instead of spending an attempt, up to
	// notReadyWait. The first attempt after it comes up runs with the normal
	// retry budget intact.
	now := time.Now().UTC()
	if isNotReady(err) && d.opts.notReadyWait > 0 {
		since := row.NotReadySince
		if since == nil {
			since = &now
		}
		if now.Before(since.Add(d.opts.notReadyWait)) {
			d.logger.Info("dispatch.subscriber_not_ready",
				slog.String("addon", sub.AddonKey), slog.String("event", row.Event), slog.String("err", err.Error()))
			pause := d.opts.retryBackoff
			if row.NotReadySince != nil {
				pause = now.Sub(*row.NotReadySince) / 2
				if pause < time.Second {
					pause = time.Second
				}
				if pause > 10*time.Second {
					pause = 10 * time.Second
				}
			}
			d.reschedule(row, pause, err.Error(), true, since)
			return
		}
	}

	var gf *guestFailure
	retryable := true
	if errors.As(err, &gf) && gf.retryable != nil {
		retryable = *gf.retryable
	}
	if retryable && attempt < d.opts.maxAttempts {
		delay := d.backoffFor(attempt + 1)
		if gf != nil && gf.retryAfter > 0 {
			delay = gf.retryAfter
		}
		d.logger.Warn("dispatch.delivery_attempt_failed",
			slog.String("addon", sub.AddonKey), slog.String("event", row.Event),
			slog.Int("attempt", attempt), slog.String("err", err.Error()))
		d.reschedule(row, delay, err.Error(), false, nil)
		return
	}

	msg := err.Error()
	switch {
	case isNotReady(err):
		// A module that never came up is not a transient failure of this
		// event: say so, otherwise the ledger only reads "not loaded".
		msg = fmt.Sprintf("%s: subscriber module never became ready within %s "+
			"(bundle without backend.wasm, compile/instantiate failure or addon disabled; see the host boot log)",
			msg, d.opts.notReadyWait)
	case !retryable:
		msg = "not retryable: " + msg
	}
	if !d.finishDead(row, msg) {
		return
	}
	d.logger.Error("dispatch.delivery_dead",
		slog.String("addon", sub.AddonKey), slog.String("event", row.Event),
		slog.String("function", sub.Function), slog.String("err", msg))
	d.notify(row.DeliveryID, row.Event, *sub, StatusDead, attempt, msg)
	d.emitDeadLetter(row.OrganizationID, row.DeliveryID, row.Event, *sub, msg)
}

// fenced returns the UPDATE scope of a row this worker still owns.
func (d *Dispatcher) fenced(r Delivery) *gorm.DB {
	return d.tbl().Where("delivery_id = ? AND lease_token = ? AND status = ?", r.DeliveryID, r.LeaseToken, StatusPending)
}

func (d *Dispatcher) finishDelivered(r Delivery, attempts int) bool {
	now := time.Now().UTC()
	res := d.fenced(r).Updates(map[string]any{
		"status": StatusDelivered, "attempts": attempts, "delivered_at": now, "last_error": "",
		"lease_until": nil, "lease_token": "", "not_ready_since": nil,
	})
	return d.owned(res, r)
}

func (d *Dispatcher) finishDead(r Delivery, msg string) bool {
	res := d.fenced(r).Updates(map[string]any{
		"status": StatusDead, "last_error": truncErr(msg),
		"lease_until": nil, "lease_token": "", "not_ready_since": nil,
	})
	return d.owned(res, r)
}

func (d *Dispatcher) finishCancelled(r Delivery, msg string) {
	res := d.fenced(r).Updates(map[string]any{
		"status": StatusCancelled, "last_error": truncErr(msg),
		"lease_until": nil, "lease_token": "", "not_ready_since": nil,
	})
	if d.owned(res, r) {
		d.notify(r.DeliveryID, r.Event, subscriptionOf(r), StatusCancelled, r.Attempts, msg)
	}
}

// reschedule releases the lease and sets the next attempt. refund gives the
// claimed attempt back (waiting for a module, a provider outage and shutdown
// are not failures of the event).
func (d *Dispatcher) reschedule(r Delivery, delay time.Duration, errMsg string, refund bool, notReadySince *time.Time) {
	attempts := r.Attempts
	if refund && attempts > 0 {
		attempts--
	}
	next := time.Now().UTC().Add(delay)
	res := d.fenced(r).Updates(map[string]any{
		"attempts": attempts, "last_error": truncErr(errMsg), "next_attempt_at": next,
		"lease_until": nil, "lease_token": "", "not_ready_since": notReadySince,
	})
	if d.owned(res, r) {
		d.armWake(delay)
	}
}

// release hands a delivery back untouched (graceful shutdown).
func (d *Dispatcher) release(r Delivery) {
	attempts := r.Attempts
	if attempts > 0 {
		attempts--
	}
	d.fenced(r).Updates(map[string]any{
		"attempts": attempts, "next_attempt_at": time.Now().UTC(), "lease_until": nil, "lease_token": "",
	})
}

func (d *Dispatcher) owned(res *gorm.DB, r Delivery) bool {
	if res.Error != nil {
		d.logger.Warn("dispatch.ledger_write_error", slog.String("delivery_id", r.DeliveryID), slog.String("err", res.Error.Error()))
		return false
	}
	if res.RowsAffected == 0 {
		d.logger.Warn("dispatch.lease_lost", slog.String("delivery_id", r.DeliveryID))
		return false
	}
	return true
}

// ReplayFilter selects the deliveries Replay re-queues.
type ReplayFilter struct {
	// OrgID restricts to one tenant; uuid.Nil = every org.
	OrgID uuid.UUID
	// AddonKey is the SUBSCRIBER whose deliveries are replayed ("" = any).
	AddonKey string
	// Event is the event name ("" = any); a trailing ".*" matches by prefix.
	Event string
	// From/To bound created_at (zero = open).
	From, To time.Time
	// Statuses defaults to delivered + dead.
	Statuses []string
	// Limit caps the rows replayed per call (0 = 1000).
	Limit int
}

// ReplayResult reports what Replay did.
type ReplayResult struct {
	Requeued  int // new generation rows created
	Existing  int // that generation already existed (no-op)
	NoPayload int // rows from before payloads were persisted; cannot be replayed
}

// Replay re-queues terminal deliveries as the NEXT generation of their chain.
// The new row has its own delivery_id (derived from the chain root and the
// generation), so it never collides with the ledger, and a second Replay of the
// same rows is a no-op until a newer generation exists. The subscriber receives
// the same payload with envelope.generation = N; any running dispatcher on the
// same table picks the rows up on its next sweep.
//
// A subscriber that is NEW and needs history should use the addon's declared
// backfills; Replay re-runs deliveries that already existed.
func Replay(ctx context.Context, db *gorm.DB, tableName string, f ReplayFilter) (ReplayResult, error) {
	var res ReplayResult
	if db == nil {
		return res, fmt.Errorf("dispatch: nil *gorm.DB")
	}
	if tableName == "" {
		tableName = DefaultTableName
	}
	statuses := f.Statuses
	if len(statuses) == 0 {
		statuses = []string{StatusDelivered, StatusDead}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 1000
	}
	q := db.Session(&gorm.Session{NewDB: true}).WithContext(ctx).Table(tableName).Where("status IN ?", statuses)
	if f.OrgID != uuid.Nil {
		q = q.Where("organization_id = ?", f.OrgID)
	}
	if f.AddonKey != "" {
		q = q.Where("addon_key = ?", f.AddonKey)
	}
	if f.Event != "" {
		if strings.HasSuffix(f.Event, ".*") {
			q = q.Where("event LIKE ?", strings.TrimSuffix(f.Event, "*")+"%")
		} else {
			q = q.Where("event = ?", f.Event)
		}
	}
	if !f.From.IsZero() {
		q = q.Where("created_at >= ?", f.From.UTC())
	}
	if !f.To.IsZero() {
		q = q.Where("created_at < ?", f.To.UTC())
	}
	var rows []Delivery
	if err := q.Order("created_at").Limit(limit).Find(&rows).Error; err != nil {
		return res, err
	}
	now := time.Now().UTC()
	for _, r := range rows {
		if r.Payload == "" {
			res.NoPayload++
			continue
		}
		root := r.RootDeliveryID
		if root == "" {
			root = r.DeliveryID
		}
		gen := r.Generation + 1
		n := Delivery{
			ID: uuid.New(), DeliveryID: replayDeliveryID(root, gen), RootDeliveryID: root, Generation: gen,
			OrganizationID: r.OrganizationID, Event: r.Event, Emitter: r.Emitter, AddonKey: r.AddonKey,
			HandlerType: r.HandlerType, Export: r.Export, SubEvent: r.SubEvent, EventID: r.EventID,
			ActorID: r.ActorID, BranchID: r.BranchID, CorrelationID: r.CorrelationID,
			Payload: r.Payload, Status: StatusPending, NextAttemptAt: &now,
		}
		ins := db.Session(&gorm.Session{NewDB: true}).WithContext(ctx).Table(tableName).
			Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "delivery_id"}}, DoNothing: true}).Create(&n)
		if ins.Error != nil {
			return res, ins.Error
		}
		if ins.RowsAffected == 0 {
			res.Existing++
		} else {
			res.Requeued++
		}
	}
	return res, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
