package dispatch

import (
	"context"
	"encoding/json"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/gorm/clause"
	"log/slog"
)

// buildEnvelope assembles the metacore.event/1 identity of one publication
// from what the HOST knows: the bus emitter, the durable publisher's Meta
// (stable id, aggregate, sequence, causation) and, for canonical CRUD events,
// the model/id the envelope already carries.
func (d *Dispatcher) buildEnvelope(ctx context.Context, orgID uuid.UUID, eventName, occurrenceID, emitter string, ce canonicalEvent, meta dynamic.Meta) Envelope {
	env := Envelope{
		Schema:        EnvelopeSchema,
		ID:            occurrenceID,
		Name:          eventName,
		Version:       meta.Version,
		Source:        emitter,
		OrgID:         orgID.String(),
		OccurredAt:    meta.OccurredAt,
		Sequence:      meta.Sequence,
		CorrelationID: ce.CorrelationID,
		CausationID:   meta.CausationID,
		ActorID:       ce.ActorID,
		BranchID:      ce.BranchID,
	}
	if env.Version <= 0 {
		env.Version = 1
	}
	if env.Source == "" {
		env.Source = "kernel"
	}
	if env.OccurredAt == "" {
		env.OccurredAt = rfc3339(time.Now())
	}
	if env.CausationID == "" {
		env.CausationID = dynamic.CausationIDFromContext(ctx)
	}
	switch {
	case meta.AggregateType != "" && meta.AggregateID != "":
		env.Aggregate = &Aggregate{Type: meta.AggregateType, ID: meta.AggregateID}
	case ce.ID != "" && ce.Model != "":
		env.Aggregate = &Aggregate{Type: ce.Model, ID: ce.ID}
	}
	return env
}

// enqueue persists the deterministic delivery row (idempotent INSERT) and, when
// the row is newly created, runs the first attempt right away on a goroutine
// holding the row's lease. A duplicate event that hits the unique index — or a
// row already delivered/dead — short-circuits with no invocation. Reports
// whether a delivery was actually enqueued, which feeds the emitter-facing
// subscriber count.
//
// The row is the durable work item (see ledger.go): if this process dies before
// or during the attempt, the lease expires and the sweeper of ANY process
// resumes it. The goroutine is only the fast path.
func (d *Dispatcher) enqueue(ctx context.Context, orgID uuid.UUID, eventName, occurrenceID, emitter string, ce canonicalEvent, payload []byte, sub Subscription) bool {
	id := deliveryID(eventName, occurrenceID, sub)
	now := time.Now().UTC()
	lease := now.Add(d.opts.lease)

	row := Delivery{
		ID:             uuid.New(),
		DeliveryID:     id,
		RootDeliveryID: id,
		OrganizationID: orgID,
		Event:          eventName,
		Emitter:        emitter,
		AddonKey:       sub.AddonKey,
		HandlerType:    sub.HandlerType,
		Export:         sub.Function,
		SubEvent:       sub.Event,
		EventID:        occurrenceID,
		ActorID:        ce.ActorID,
		BranchID:       ce.BranchID,
		CorrelationID:  ce.CorrelationID,
		Payload:        string(payload),
		Status:         StatusPending,
		// The fast path claims the row itself: attempt 1, lease held.
		Attempts:      1,
		NextAttemptAt: &now,
		LeaseUntil:    &lease,
		LeaseToken:    newClaimToken(d.workerID),
	}

	// Idempotent insert: ON CONFLICT(delivery_id) DO NOTHING. RowsAffected==0
	// means the delivery already exists (a re-published event) — do not
	// re-invoke. This is the day-one idempotency guarantee.
	res := d.tbl().
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "delivery_id"}}, DoNothing: true}).
		Create(&row)
	if res.Error != nil {
		d.logger.Warn("dispatch.ledger_insert_error",
			slog.String("delivery_id", id),
			slog.String("err", res.Error.Error()))
		return false
	}
	if res.RowsAffected == 0 {
		// Already enqueued/delivered/dead — idempotent no-op.
		return false
	}

	if d.baseCtx.Err() != nil {
		// Shut down between the insert and the attempt: leave the row leased;
		// the next process resumes it when the lease expires.
		return true
	}
	d.inflight.Add(1)
	go func() {
		defer d.inflight.Add(-1)
		d.runClaimed(row, &sub)
	}()
	return true
}

// guestFailure is the error a `{success:false}` guest answer becomes. It keeps
// the guest's own verdict on whether the failure is worth retrying:
//
//	{"success":false, "retryable":true,  "retry_after_ms":30000}  retry, honour the delay
//	{"success":false, "retryable":false}                          dead-letter now
//	{"success":false}                                             legacy: retried up to MaxAttempts
//
// The last form is deliberately unchanged so subscribers written before
// `retryable` existed behave exactly as they did.
type guestFailure struct {
	msg        string
	retryable  *bool
	retryAfter time.Duration
}

func (e *guestFailure) Error() string { return e.msg }

func newGuestFailure(msg string, raw []byte) *guestFailure {
	f := &guestFailure{msg: msg}
	var e struct {
		Retryable    *bool  `json:"retryable"`
		RetryAfterMs *int64 `json:"retry_after_ms"`
		Error        *struct {
			Retryable    *bool  `json:"retryable"`
			RetryAfterMs *int64 `json:"retry_after_ms"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return f
	}
	f.retryable, f.retryAfter = e.Retryable, msToDuration(e.RetryAfterMs)
	if e.Error != nil {
		if f.retryable == nil {
			f.retryable = e.Error.Retryable
		}
		if f.retryAfter == 0 {
			f.retryAfter = msToDuration(e.Error.RetryAfterMs)
		}
	}
	return f
}

// maxRetryAfter caps a guest-requested delay so a bad value cannot park a
// delivery for days.
const maxRetryAfter = time.Hour

func msToDuration(ms *int64) time.Duration {
	if ms == nil || *ms <= 0 {
		return 0
	}
	d := time.Duration(*ms) * time.Millisecond
	if d > maxRetryAfter || d < 0 {
		return maxRetryAfter
	}
	return d
}
