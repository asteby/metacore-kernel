package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
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

	// baseCtx lives as long as the dispatcher (cancel func of Wire stops it);
	// every attempt derives from it, so shutdown interrupts running handlers
	// and hands their deliveries back without spending an attempt.
	baseCtx context.Context
	stop    context.CancelFunc
	// workerID prefixes lease tokens so a stuck row names its last holder.
	workerID string
	// inflight counts attempts, sweeps and armed wake-ups; running counts the
	// attempts the sweeper started (bounds its concurrency). WaitIdle is
	// test-facing.
	inflight atomic.Int64
	running  atomic.Int64
}

// canonicalEvent is the dispatcher's structural view of the bus payload. It
// matches dynamic.CanonicalEvent's JSON shape without importing the dynamic
// package (avoids a dependency cycle: dynamic could one day Wire the
// dispatcher). The dispatcher only needs the envelope identity to build the
// delivery key + the JSON it forwards to handlers; it re-marshals the original
// payload so guests receive the full canonical event.
type canonicalEvent struct {
	ID            string `json:"id"`
	OccurrenceID  string `json:"occurrence_id"`
	Model         string `json:"model"`
	Action        string `json:"action"`
	AddonKey      string `json:"addon_key"`
	ActorID       string `json:"actor_id"`
	BranchID      string `json:"branch_id"`
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
	// Stamp the event NAME into the delivered JSON when the payload object
	// doesn't carry one. A guest-emitted domain event (event_emit) forwards
	// only the emitter's payload — the subscriber had NO WAY to know which
	// event woke it up unless the emitter remembered to duplicate the name
	// inside the payload. In prod this made workshop's on_stock_picked
	// silently no-op ("handled:false, event:\"\"") on every real
	// warehouse.stock_picked delivery, leaving the 2-minute healer as the
	// only thing opening work orders. Canonical events are untouched (they
	// self-describe via model/action, and this only adds the key when the
	// object lacks it).
	raw = stampEventName(raw, eventName)
	var ce canonicalEvent
	_ = json.Unmarshal(raw, &ce) // best-effort; a non-canonical payload leaves it zero
	// Chain the actor through multi-hop event flows: a DOMAIN event emitted by
	// a guest via event_emit (e.g. warehouse.stock_picked) carries no actor_id
	// in its payload, but the emitting delivery's ctx does — Publish forwards
	// it here. Without this fallback the NEXT hop's subscriber (workshop
	// opening a WorkOrder off the pick) ran actorless and its rows audited as
	// "Sistema"/N/A even though a real user started the chain.
	//
	// The actor also has to reach the GUEST, not just the delivery ctx: a
	// domain event is guest-authored, so the host's ctx actor wins over any
	// actor_id the emitter wrote and is stamped into the payload the
	// subscriber receives. A canonical event is host-built (publishCanonical)
	// and already carries its actor, so it is only filled in when absent.
	if actor := dynamic.ActorIDFromContext(ctx); actor != "" {
		canonical := ce.ID != "" && ce.Model != ""
		if !canonical || ce.ActorID == "" {
			ce.ActorID = actor
			raw = stampActorID(raw, actor)
		}
	}

	// Branch, with the opposite precedence to the actor: a branch the payload
	// already names wins — the document's own branch on a canonical event, or
	// one a domain-event emitter wrote on purpose — and only when it names
	// none (or a non-uuid) is the emitting ctx's active branch filled in, into
	// the payload AND the delivery ctx (enqueue). That way a subscriber's
	// creates (data_mutate stamps branch_id from the ctx) land in the branch
	// of the document or user that started the chain.
	if id, err := uuid.Parse(ce.BranchID); err != nil || id == uuid.Nil {
		ce.BranchID = ""
		if b := dynamic.BranchIDFromContext(ctx); b != "" {
			ce.BranchID = b
			raw = stampString(raw, "branch_id", b)
		}
	}

	// Model-key guard (see model_key_guard.go). A publisher that named the
	// event after the table instead of the manifest ModelKey routes to nobody
	// and says nothing about it; this warns and routes to the canonical name.
	// Inert until a host wires WithModelKeyResolver.
	eventName = d.checkModelKey(ctx, eventName, ce)

	// occurrenceID is the idempotency discriminator for this PUBLICATION. It
	// must distinguish two different updates of the same row while staying
	// identical across a re-delivery of one update — that tension is the whole
	// design, and it used to be resolved the wrong way: the discriminator was
	// the mutated row id, so the FIRST update of a row consumed the only
	// delivery that row would ever get and every later update was silently
	// dropped as a "re-publish" (a credit sale never reached its settled
	// handler, an account statement froze after the first payment).
	//
	// dynamic.publishCanonical now stamps a per-publication occurrence_id, and
	// the outbox relay re-publishes the PERSISTED bytes, so a replayed event
	// carries the same id and still collapses to one delivery.
	meta := dynamic.MetaFromContext(ctx)
	occurrenceID := ce.OccurrenceID
	if occurrenceID == "" {
		// A durable publisher (the wasm event_emit buffer, the outbox relay)
		// stamped a stable publication id: a re-publish of the persisted event
		// carries the same id, so the ledger collapses it into the deliveries
		// it already recorded.
		occurrenceID = meta.ID
	}
	if occurrenceID == "" && ce.ID != "" {
		// Canonical event from before occurrence_id existed (an unpublished
		// outbox row written by an older kernel, or a host building the
		// envelope by hand): fingerprint the serialized payload. Byte-stable
		// across a replay of the same publication, and different between two
		// updates because `after` — updated_at included — differs.
		sum := sha256.Sum256(raw)
		occurrenceID = ce.ID + ":" + hex.EncodeToString(sum[:])
	}
	if occurrenceID == "" {
		// A domain event (event_emit) has no natural key at all; reusing an
		// empty string would make every later emission of the same name a
		// permanent no-op, so each publication is delivered on its own merits.
		occurrenceID = uuid.NewString()
	}

	// Emitter: who published this event, as the BUS saw it (the addonKey of
	// Publish, carried in ctx by events.PublishWithCount) — never what the
	// payload claims. It is stamped into every JSON-object payload under
	// "emitter", overwriting a value the guest wrote, so a subscriber such as
	// channel_orders can refuse a storefront_cart.checkout_paid that
	// storefront_cart did not publish. Stamped AFTER the occurrence id is
	// derived: the legacy fingerprint above hashes the payload, and an outbox
	// row written by an older kernel must keep fingerprinting the same bytes.
	emitter := events.EmitterFromContext(ctx)
	if emitter != "" {
		raw = stampString(raw, EmitterField, emitter)
	}

	subs, err := d.provider.SubscriptionsForOrg(ctx, orgID)
	if err != nil {
		d.logger.Warn("dispatch.provider_error",
			slog.String("org", orgID.String()),
			slog.String("event", eventName),
			slog.String("err", err.Error()))
		// Durable router: surface the failure so the outbox does not stamp
		// published_at and the relay retries (the ledger dedups by stable id).
		return 0, fmt.Errorf("dispatch: subscriptions for org %s: %w", orgID, err)
	}

	if ce.CorrelationID == "" {
		ce.CorrelationID = dynamic.CorrelationIDFromContext(ctx)
	}
	// metacore.event/1 identity block (additive; see envelope.go).
	raw = stampEnvelope(raw, d.buildEnvelope(ctx, orgID, eventName, occurrenceID, emitter, ce, meta))

	enqueued := 0
	var errs []error
	for i := range subs {
		sub := subs[i]
		if !eventMatches(sub.Event, eventName) {
			continue
		}
		ok, err := d.enqueue(ctx, orgID, eventName, occurrenceID, emitter, ce, raw, sub)
		if err != nil {
			// Keep going: the other subscriptions still get their rows; the
			// retry only re-inserts what is missing (stable delivery ids).
			errs = append(errs, err)
			continue
		}
		if ok {
			enqueued++
		}
	}
	return enqueued, errors.Join(errs...)
}

// estimate is the events.Estimator of the dispatcher's "*" tap: the number of
// deliveries handle WOULD enqueue for eventName in orgID (subscriptions of the
// org whose pattern matches), without persisting anything. ok=false when the
// provider fails, so the caller reports "unknown" instead of a wrong number.
func (d *Dispatcher) estimate(ctx context.Context, orgID uuid.UUID, eventName string) (int, bool) {
	subs, err := d.provider.SubscriptionsForOrg(ctx, orgID)
	if err != nil {
		return 0, false
	}
	n := 0
	for i := range subs {
		if eventMatches(subs[i].Event, eventName) {
			n++
		}
	}
	return n, true
}

// NotReadyError is implemented by invoker errors that mean "the subscriber
// cannot run YET" (runtime still loading) rather than "the subscriber
// failed". The dispatcher waits for it instead of spending attempts.
type NotReadyError interface {
	error
	NotReady() bool
}

func isNotReady(err error) bool {
	var nr NotReadyError
	return errors.As(err, &nr) && nr.NotReady()
}

// DeadLetterEvent is published on the bus whenever a delivery exhausts its
// retries and lands in the dead-letter state. Hosts can subscribe (or declare
// a notification rule over it) so a dead delivery pages a human instead of
// waiting to be discovered in the ledger. The payload is a flat map:
// {delivery_id, addon_key, event, function, error}.
const DeadLetterEvent = "kernel.dispatch.delivery_dead"

// emitDeadLetter publishes DeadLetterEvent, guarding against recursion: a
// dead delivery OF the dead-letter event itself must not emit another one,
// or two chronically-failing subscribers could ping-pong forever.
func (d *Dispatcher) emitDeadLetter(orgID uuid.UUID, id, eventName string, sub Subscription, errMsg string) {
	if d.bus == nil || eventName == DeadLetterEvent {
		return
	}
	_ = d.bus.Publish(context.Background(), "kernel", DeadLetterEvent, orgID, map[string]any{
		"delivery_id": id,
		"addon_key":   sub.AddonKey,
		"event":       eventName,
		"function":    sub.Function,
		"error":       errMsg,
	})
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
			return newGuestFailure("guest: "+msg, out)
		}
		return nil
	case handlerTypeNative:
		if d.opts.native == nil {
			return errors.New("native subscription but no NativeInvoker wired")
		}
		out, err := d.opts.native.Invoke(ctx, orgID, sub.Installation, sub.AddonKey, sub.Function, payload)
		if err != nil {
			return err
		}
		if msg := guestEnvelopeError(out); msg != "" {
			d.logger.Warn("dispatch.native_error",
				slog.String("addon", sub.AddonKey),
				slog.String("operation", sub.Function),
				slog.String("err", msg))
			return newGuestFailure("native: "+msg, out)
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

// WaitIdle blocks until no delivery attempt, sweep or scheduled retry wake-up
// is in flight. Test-facing / graceful-shutdown helper — production event paths
// fire-and-forget. It does NOT prevent new deliveries from being enqueued; call
// it after the bus has stopped publishing. A retry scheduled further out than
// the poll interval is durable state (next_attempt_at), not in-flight work, so
// it does not hold WaitIdle.
func (d *Dispatcher) WaitIdle() {
	quiet := 0
	for quiet < 3 {
		if d.inflight.Load() == 0 {
			quiet++
		} else {
			quiet = 0
		}
		time.Sleep(time.Millisecond)
	}
}

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
			Code       string `json:"code"`
			Message    string `json:"message"`
			MessageKey string `json:"message_key"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Success == nil || *e.Success {
		return ""
	}
	if e.Error == nil {
		return "success=false"
	}
	msg := unwrapHostEnvelope(e.Error.Message)
	out := e.Error.Code
	switch {
	case out != "" && msg != "":
		out += ": " + msg
	case msg != "":
		out = msg
	case out == "":
		out = "success=false"
	}
	if e.Error.MessageKey != "" {
		out += " [" + e.Error.MessageKey + "]"
	}
	return out
}

// unwrapHostEnvelope makes a guest message that embeds a raw host error
// envelope (`upsert x: {"error":{"code":"db_error","message":"ERROR: …"},
// "meta":{…},"success":false}` — guests often append the host response
// verbatim) readable: the JSON is replaced by `host <code>: <message>`.
// Anything that does not parse as such an envelope is returned unchanged.
func unwrapHostEnvelope(msg string) string {
	i := strings.Index(msg, `{"error"`)
	if i < 0 {
		return msg
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(msg[i:]))
	if err := dec.Decode(&env); err != nil || (env.Error.Code == "" && env.Error.Message == "") {
		return msg
	}
	rest := strings.TrimSpace(msg[i+int(dec.InputOffset()):])
	out := msg[:i] + "host " + env.Error.Code + ": " + env.Error.Message
	if rest != "" {
		out += " " + rest
	}
	return out
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

// stampEventName injects {"event": name} into a JSON OBJECT payload that does
// not already carry an "event" key, so subscribers can always identify the
// delivery. Non-object payloads (arrays, scalars) and objects that already
// name their event pass through untouched.
func stampEventName(raw []byte, eventName string) []byte {
	if eventName == "" {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw
	}
	if _, has := obj["event"]; has {
		return raw
	}
	nameJSON, err := json.Marshal(eventName)
	if err != nil {
		return raw
	}
	obj["event"] = nameJSON
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// EmitterField is the top-level key the dispatcher stamps into every
// JSON-object payload it delivers: the addon key (or "kernel") that published
// the event on the bus. Host-authored; a value the emitter wrote is replaced.
const EmitterField = "emitter"

// stampActorID sets the top-level `actor_id` of a JSON-object payload,
// replacing whatever was there. Non-object payloads are returned untouched
// (the actor still rides on the delivery ctx).
func stampActorID(raw []byte, actor string) []byte {
	return stampString(raw, "actor_id", actor)
}

// stampString sets a top-level string key of a JSON-object payload, replacing
// whatever was there. Non-object payloads are returned untouched.
func stampString(raw []byte, key, value string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw
	}
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	obj[key] = valueJSON
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}
