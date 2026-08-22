package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Dispatcher is the runtime built by Wire. It is the events.Handler registered
// under the trusted "kernel" key plus the per-tier delivery machinery. Not
// constructed directly outside this package — use Wire.
type Dispatcher struct {
	bus      *events.Bus
	invoker  WasmInvoker
	db       *gorm.DB
	provider SubscriptionProvider
	opts     *Options
	logger   *slog.Logger

	// wg tracks in-flight async deliveries so tests (and graceful shutdown)
	// can wait for the queue to drain. Production hosts fire-and-forget; the
	// WaitIdle helper is test-facing.
	wg sync.WaitGroup
}

// canonicalEvent is the dispatcher's structural view of the bus payload. It
// matches dynamic.CanonicalEvent's JSON shape without importing the dynamic
// package (avoids a dependency cycle: dynamic could one day Wire the
// dispatcher). The dispatcher only needs the envelope identity to build the
// delivery key + the JSON it forwards to handlers; it re-marshals the original
// payload so guests receive the full canonical event.
type canonicalEvent struct {
	ID            string `json:"id"`
	Model         string `json:"model"`
	Action        string `json:"action"`
	AddonKey      string `json:"addon_key"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

// handle is the events.RoutingHandler the dispatcher registers under "*". It
// runs synchronously inside the bus fan-out (like every handler), so it MUST
// stay cheap: it resolves matching subscriptions, persists each delivery row,
// and hands the actual handler invocation off to a worker goroutine. The CRUD
// mutation that produced the event is therefore never blocked by a subscriber.
//
// It returns the number of deliveries enqueued so the bus can report an honest
// subscriber count to the emitter (a guest calling `event_emit` gets that count
// back in its envelope; before, it always saw 1 — this wildcard tap — even when
// nothing was routed).
func (d *Dispatcher) handle(ctx context.Context, orgID uuid.UUID, eventName string, payload any) (int, error) {
	// The event NAME comes from the bus and is authoritative: it is what the
	// emitter passed to Publish / `event_emit`. The payload is re-marshalled
	// only to (a) produce the JSON bytes forwarded to handlers and (b) read the
	// canonical envelope's identity fields (event id, actor, correlation) when
	// the payload happens to be a CanonicalEvent. A payload that is NOT
	// canonical routes exactly the same way — that is the whole point: domain
	// events emitted by guests ("pos.order_created") are first-class here.
	//
	// Note the name is never reconstructed from the payload any more. A domain
	// payload that coincidentally carries `model`/`action` fields used to be
	// re-routed under `<addon>.<Model>.<Action>`, silently ignoring the name the
	// emitter chose. For canonical CRUD events the two are identical by
	// construction (dynamic.publishCanonical builds the name from the same
	// addonKey/model/action it puts in the envelope), so canonical routing is
	// unchanged.
	if eventName == "" {
		return 0, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil
	}
	var ce canonicalEvent
	_ = json.Unmarshal(raw, &ce) // best-effort; a non-canonical payload leaves it zero

	// occurrenceID is the idempotency discriminator for this publication. A
	// canonical event has a natural one (the mutated row id), which is what
	// makes a re-publish collapse to a single delivery. A domain event has no
	// such key, and reusing an empty string would make every subsequent
	// emission of the same event name a permanent no-op — so each publication
	// gets a fresh id and is delivered on its own merits.
	occurrenceID := ce.ID
	if occurrenceID == "" {
		occurrenceID = uuid.NewString()
	}

	subs, err := d.provider.SubscriptionsForOrg(ctx, orgID)
	if err != nil {
		d.logger.Warn("dispatch.provider_error",
			slog.String("org", orgID.String()),
			slog.String("event", eventName),
			slog.String("err", err.Error()))
		return 0, nil
	}

	enqueued := 0
	for i := range subs {
		sub := subs[i]
		if !eventMatches(sub.Event, eventName) {
			continue
		}
		if d.enqueue(ctx, orgID, eventName, occurrenceID, ce, raw, sub) {
			enqueued++
		}
	}
	return enqueued, nil
}

// enqueue persists the deterministic delivery row (idempotent INSERT) and, when
// the row is newly created (or still pending), launches the async worker. A
// duplicate event that hits the unique index — or a row already delivered/dead —
// short-circuits with no invocation. Reports whether a delivery was actually
// enqueued, which feeds the emitter-facing subscriber count.
func (d *Dispatcher) enqueue(ctx context.Context, orgID uuid.UUID, eventName, occurrenceID string, ce canonicalEvent, payload []byte, sub Subscription) bool {
	id := deliveryID(eventName, occurrenceID, sub)

	row := Delivery{
		ID:             uuid.New(),
		DeliveryID:     id,
		OrganizationID: orgID,
		Event:          eventName,
		AddonKey:       sub.AddonKey,
		HandlerType:    sub.HandlerType,
		Export:         sub.Function,
		Status:         StatusPending,
		Attempts:       0,
	}

	// Idempotent insert: ON CONFLICT(delivery_id) DO NOTHING. RowsAffected==0
	// means the delivery already exists (a re-published event) — do not
	// re-invoke. This is the day-one idempotency guarantee.
	res := d.db.Session(&gorm.Session{NewDB: true}).
		Table(d.opts.tableName).
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

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		// Detach from the request ctx: the source HTTP request may complete
		// (and cancel its ctx) long before a slow subscriber finishes. Carry a
		// fresh background ctx with a generous per-delivery deadline — but
		// re-attach the originating EVENT's identity (actor + correlation):
		// host imports the handler calls (data_mutate above all) stamp their
		// own canonical events from this ctx, so without it every downstream
		// mutation surfaced as an anonymous actor in the audit trail.
		bg, cancel := context.WithTimeout(context.Background(), d.deliveryTimeout())
		defer cancel()
		bg = dynamic.WithActorID(bg, ce.ActorID)
		if ce.CorrelationID != "" {
			bg = dynamic.WithCorrelationID(bg, ce.CorrelationID)
		}
		d.deliver(bg, orgID, eventName, id, payload, sub)
	}()
	return true
}

// deliveryTimeout bounds a single delivery's wall clock across its retries.
// The wasm runtime already caps each invocation via the BackendSpec timeout;
// this is the outer guard so a retry loop cannot run unbounded.
func (d *Dispatcher) deliveryTimeout() time.Duration {
	// MaxAttempts invocations, each bounded by the runtime; a generous outer
	// envelope. Kept simple — not configurable until a host needs it.
	return time.Duration(d.opts.maxAttempts) * 30 * time.Second
}

// deliver runs the bounded retry loop for one delivery. Each attempt:
// authorize → invoke the tier handler → on success mark delivered, on error
// bump attempts and (if exhausted) mark dead.
func (d *Dispatcher) deliver(ctx context.Context, orgID uuid.UUID, eventName, id string, payload []byte, sub Subscription) {
	// Capability gate FIRST — an addon that lacks event:subscribe never has
	// its handler invoked, and the denial is a terminal (dead) outcome, not a
	// retryable error (retrying a permission denial is pointless).
	if d.opts.capability != nil {
		if err := d.opts.capability.CanDeliver(sub.AddonKey, eventName); err != nil {
			msg := fmt.Sprintf("capability denied: %v", err)
			d.markDead(id, msg)
			d.logger.Warn("dispatch.capability_denied",
				slog.String("addon", sub.AddonKey),
				slog.String("event", eventName))
			d.notify(id, eventName, sub, StatusDead, 0, msg)
			return
		}
	}

	var lastErr error
	for attempt := 1; attempt <= d.opts.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		// Space out retries. The wait is skipped on the first attempt and
		// aborts early if the delivery's outer deadline expires while waiting.
		if attempt > 1 {
			if err := sleepCtx(ctx, d.backoffFor(attempt)); err != nil {
				lastErr = err
				break
			}
		}
		err := d.invoke(ctx, orgID, payload, sub)
		if err == nil {
			d.markDelivered(id, attempt)
			d.logger.Debug("dispatch.delivered",
				slog.String("addon", sub.AddonKey),
				slog.String("event", eventName),
				slog.String("function", sub.Function),
				slog.Int("attempt", attempt))
			d.notify(id, eventName, sub, StatusDelivered, attempt, "")
			return
		}
		lastErr = err
		d.bumpAttempt(id, attempt, err.Error())
		d.logger.Warn("dispatch.delivery_attempt_failed",
			slog.String("addon", sub.AddonKey),
			slog.String("event", eventName),
			slog.Int("attempt", attempt),
			slog.String("err", err.Error()))
	}

	msg := "exhausted retries"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	d.markDead(id, msg)
	d.logger.Error("dispatch.delivery_dead",
		slog.String("addon", sub.AddonKey),
		slog.String("event", eventName),
		slog.String("function", sub.Function),
		slog.String("err", msg))
	d.notify(id, eventName, sub, StatusDead, d.opts.maxAttempts, msg)
}

// backoffFor returns the wait before `attempt` (which is always >= 2):
// base * 2^(attempt-2), capped at retryBackoffMax, with up to +/-20% jitter so
// a burst of deliveries that failed together does not retry in lockstep.
func (d *Dispatcher) backoffFor(attempt int) time.Duration {
	base := d.opts.retryBackoff
	if base <= 0 {
		return 0
	}
	wait := base << (attempt - 2)
	if wait <= 0 || wait > d.opts.retryBackoffMax {
		wait = d.opts.retryBackoffMax
	}
	// Deterministic-enough jitter; crypto randomness is pointless here.
	jitter := time.Duration(rand.Int63n(int64(wait/5)*2+1)) - wait/5
	if wait+jitter > 0 {
		wait += jitter
	}
	return wait
}

// sleepCtx waits for d unless ctx ends first, in which case it returns the
// ctx error so the caller can stop retrying.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// notify fires the optional WithOnDelivery observer for a terminal delivery.
func (d *Dispatcher) notify(id, eventName string, sub Subscription, status string, attempts int, errMsg string) {
	if d.opts.onDelivery == nil {
		return
	}
	d.opts.onDelivery(DeliveryResult{
		DeliveryID: id,
		AddonKey:   sub.AddonKey,
		Event:      eventName,
		Function:   sub.Function,
		Status:     status,
		Attempts:   attempts,
		Err:        errMsg,
	})
}

// invoke routes one attempt to the correct tier. Returns an error when the
// invocation itself failed (retryable) OR when a wasm guest returns a
// `{success:false}` envelope. A skipped-but-handled guest must return
// `{success:true, data:{skipped:...}}` — treating success:false as delivered
// hid production create failures (empty last_error, no work order).
func (d *Dispatcher) invoke(ctx context.Context, orgID uuid.UUID, payload []byte, sub Subscription) error {
	switch strings.ToLower(sub.HandlerType) {
	case handlerTypeWasm:
		if d.invoker == nil {
			return errors.New("wasm subscription but no WasmInvoker wired")
		}
		out, err := d.invoker.InvokeFor(ctx, orgID, sub.Installation, sub.AddonKey, sub.Function, payload, sub.Settings)
		if err != nil {
			return err
		}
		if msg := guestEnvelopeError(out); msg != "" {
			d.logger.Warn("dispatch.guest_error",
				slog.String("addon", sub.AddonKey),
				slog.String("function", sub.Function),
				slog.String("err", msg))
			return fmt.Errorf("guest: %s", msg)
		}
		return nil
	case handlerTypeCompiled:
		if d.opts.compiled == nil {
			return ErrNoCompiledRegistry
		}
		h, ok := d.opts.compiled.Lookup(sub.Function)
		if !ok {
			return fmt.Errorf("compiled handler %q not registered", sub.Function)
		}
		return h.Handle(ctx, orgID, payload)
	default:
		// "webhook" or unknown tier — terminal, not retryable. Surface as an
		// error so the delivery dead-letters with a clear reason.
		return fmt.Errorf("unsupported handler type %q", sub.HandlerType)
	}
}

func (d *Dispatcher) markDelivered(id string, attempts int) {
	now := time.Now().UTC()
	d.db.Session(&gorm.Session{NewDB: true}).
		Table(d.opts.tableName).
		Where("delivery_id = ?", id).
		Updates(map[string]any{
			"status":       StatusDelivered,
			"attempts":     attempts,
			"delivered_at": now,
			"last_error":   "",
		})
}

func (d *Dispatcher) bumpAttempt(id string, attempts int, errMsg string) {
	d.db.Session(&gorm.Session{NewDB: true}).
		Table(d.opts.tableName).
		Where("delivery_id = ?", id).
		Updates(map[string]any{
			"attempts":   attempts,
			"last_error": truncErr(errMsg),
		})
}

func (d *Dispatcher) markDead(id, errMsg string) {
	d.db.Session(&gorm.Session{NewDB: true}).
		Table(d.opts.tableName).
		Where("delivery_id = ?", id).
		Updates(map[string]any{
			"status":     StatusDead,
			"last_error": truncErr(errMsg),
		})
}

// WaitIdle blocks until all in-flight async deliveries finish. Test-facing /
// graceful-shutdown helper — production event paths fire-and-forget. It does
// NOT prevent new deliveries from being enqueued; call it after the bus has
// stopped publishing.
func (d *Dispatcher) WaitIdle() { d.wg.Wait() }

// guestEnvelopeError returns a non-empty message when the wasm guest returned
// a `{success:false}` envelope. Missing/malformed JSON is not a guest error
// (older compiled fixtures return raw bytes).
func guestEnvelopeError(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var e struct {
		Success *bool `json:"success"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Success == nil || *e.Success {
		return ""
	}
	if e.Error != nil {
		if e.Error.Code != "" && e.Error.Message != "" {
			return e.Error.Code + ": " + e.Error.Message
		}
		if e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Error.Code != "" {
			return e.Error.Code
		}
	}
	return "success=false"
}

// eventMatches reuses the exact bus wildcard rule (exact, "*", trailing ".*")
// so subscription authors learn one matching semantics across the platform.
func eventMatches(pattern, event string) bool {
	if pattern == event || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, ".*")
		if event == prefix {
			return true
		}
		return strings.HasPrefix(event, prefix+".")
	}
	return false
}

// truncErr caps a stored error string so a pathological driver message cannot
// bloat the ledger row.
func truncErr(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}
