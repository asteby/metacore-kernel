package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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
	ID       string `json:"id"`
	Model    string `json:"model"`
	Action   string `json:"action"`
	AddonKey string `json:"addon_key"`
}

// handle is the events.Handler the dispatcher registers under "*". It runs
// synchronously inside the bus fan-out (like every handler), so it MUST stay
// cheap: it resolves matching subscriptions, persists each delivery row, and
// hands the actual handler invocation off to a worker goroutine. The CRUD
// mutation that produced the event is therefore never blocked by a subscriber.
func (d *Dispatcher) handle(ctx context.Context, orgID uuid.UUID, payload any) error {
	// Re-marshal the payload to (a) extract the canonical identity and (b)
	// produce the JSON bytes forwarded to handlers. A non-canonical / unmarshalable
	// payload is ignored — domain events emitted by guests flow through the same
	// bus but carry their own shape; subscriptions still match on the event NAME,
	// which the bus passed to us implicitly via the matched subscription. However,
	// we cannot see the event name here (the bus does not pass it to Handler), so
	// the dispatcher keys on the canonical envelope's reconstructed name. See note.
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var ce canonicalEvent
	if err := json.Unmarshal(raw, &ce); err != nil || ce.Model == "" || ce.Action == "" {
		// Not a canonical CRUD event (e.g. a secondary domain event a guest
		// emitted). The dispatcher only routes canonical events; domain-event
		// routing keys on a name the Handler signature does not expose, so it
		// is out of scope for this layer (a future bus revision that passes the
		// event name to Handler unlocks it).
		return nil
	}
	addonKey := ce.AddonKey
	if addonKey == "" {
		addonKey = "kernel"
	}
	eventName := fmt.Sprintf("%s.%s.%s", addonKey, ce.Model, ce.Action)

	subs, err := d.provider.SubscriptionsForOrg(ctx, orgID)
	if err != nil {
		d.logger.Warn("dispatch.provider_error",
			slog.String("org", orgID.String()),
			slog.String("event", eventName),
			slog.String("err", err.Error()))
		return nil
	}

	for i := range subs {
		sub := subs[i]
		if !eventMatches(sub.Event, eventName) {
			continue
		}
		d.enqueue(ctx, orgID, eventName, ce.ID, raw, sub)
	}
	return nil
}

// enqueue persists the deterministic delivery row (idempotent INSERT) and, when
// the row is newly created (or still pending), launches the async worker. A
// duplicate event that hits the unique index — or a row already delivered/dead —
// short-circuits with no invocation.
func (d *Dispatcher) enqueue(ctx context.Context, orgID uuid.UUID, eventName, rowID string, payload []byte, sub Subscription) {
	id := deliveryID(eventName, rowID, sub)

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
		return
	}
	if res.RowsAffected == 0 {
		// Already enqueued/delivered/dead — idempotent no-op.
		return
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		// Detach from the request ctx: the source HTTP request may complete
		// (and cancel its ctx) long before a slow subscriber finishes. Carry a
		// fresh background ctx with a generous per-delivery deadline.
		bg, cancel := context.WithTimeout(context.Background(), d.deliveryTimeout())
		defer cancel()
		d.deliver(bg, orgID, eventName, id, payload, sub)
	}()
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

// invoke routes one attempt to the correct tier. Returns an error only when the
// invocation itself failed (retryable); a clean return — even of a guest-level
// error envelope — counts as delivered.
func (d *Dispatcher) invoke(ctx context.Context, orgID uuid.UUID, payload []byte, sub Subscription) error {
	switch strings.ToLower(sub.HandlerType) {
	case handlerTypeWasm:
		if d.invoker == nil {
			return errors.New("wasm subscription but no WasmInvoker wired")
		}
		_, err := d.invoker.InvokeFor(ctx, orgID, sub.Installation, sub.AddonKey, sub.Function, payload, sub.Settings)
		return err
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
