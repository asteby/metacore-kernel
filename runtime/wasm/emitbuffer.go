package wasm

// Emit-on-commit for the guest's event_emit import.
//
// event_emit used to publish to the bus the instant the guest called it. The
// ABI has no transaction, so an event emitted halfway through an invocation
// went out even when the invocation then failed — announcing a state that was
// never saved — and a process crash between the emit and the fan-out lost it.
//
// Now event_emit only VALIDATES (capability, emit policy, size) and appends to
// a per-invocation buffer. When the invocation ends:
//
//   - trap / error / {"success":false}  -> the buffer is DISCARDED: no outbox
//     row, no delivery, the event never happened;
//   - success                           -> the buffered events are written to
//     kernel_event_outbox and only then published (dynamic.OutboxPublisher), so a
//     crash between the two is recovered by the outbox relay.
//
// An answer that is not a JSON envelope with an explicit `success:false` counts
// as success: guests that predate the envelope return raw bytes.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	// maxStagedEvents / maxStagedBytes bound what one invocation may buffer, so
	// a runaway guest cannot hold unbounded memory until it returns.
	maxStagedEvents = 256
	maxStagedBytes  = 4 << 20
)

type emitBuffer struct {
	mu    sync.Mutex
	evs   []dynamic.StagedEvent
	bytes int
}

type emitBufKey struct{}

func withEmitBuffer(ctx context.Context, b *emitBuffer) context.Context {
	return context.WithValue(ctx, emitBufKey{}, b)
}

func emitBufferFrom(ctx context.Context) *emitBuffer {
	b, _ := ctx.Value(emitBufKey{}).(*emitBuffer)
	return b
}

func (b *emitBuffer) add(ev dynamic.StagedEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.evs) >= maxStagedEvents {
		return fmt.Errorf("invocation already emitted %d events", maxStagedEvents)
	}
	if b.bytes+len(ev.Payload) > maxStagedBytes {
		return fmt.Errorf("invocation emitted more than %d bytes of events", maxStagedBytes)
	}
	b.evs = append(b.evs, ev)
	b.bytes += len(ev.Payload)
	return nil
}

func (b *emitBuffer) reset() {
	b.mu.Lock()
	b.evs, b.bytes = nil, 0
	b.mu.Unlock()
}

func (b *emitBuffer) take() []dynamic.StagedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.evs
	b.evs, b.bytes = nil, 0
	return out
}

// invocationCommitted reports whether a guest answer means "the work stands".
func invocationCommitted(out []byte) bool {
	var e struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal(out, &e) != nil || e.Success == nil {
		return true
	}
	return *e.Success
}

// maxAggregateField bounds aggregate.type / aggregate.id; both end up in the
// sequence table's primary key.
const maxAggregateField = 128

var aggregateFieldRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

func validAggregateField(s string) bool {
	return len(s) > 0 && len(s) <= maxAggregateField && aggregateFieldRe.MatchString(s)
}

// stageEmit builds the StagedEvent for one event_emit call, stamping the host
// identity (id, time, causation, actor, branch, correlation) from the call's
// ctx and reading the optional aggregate the guest declared in its payload:
//
//	{"aggregate": {"type": "SalesOrder", "id": "…"}, …}
//
// A declared aggregate must have a type and an id of 1..128 bytes matching
// [A-Za-z0-9_.:-]+; anything else is an error (the guest gets invalid_payload).
// Its sequence counter is namespaced by the emitting addon (see
// dynamic.OutboxPublisher), so a guest cannot advance another emitter's counters.
func stageEmit(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload []byte) (dynamic.StagedEvent, error) {
	ev := dynamic.StagedEvent{
		ID:            dynamic.NewEventID(),
		AddonKey:      addonKey,
		Name:          event,
		OrgID:         orgID,
		Payload:       payload,
		Version:       1,
		OccurredAt:    time.Now().UTC(),
		CausationID:   dynamic.CausationIDFromContext(ctx),
		CorrelationID: dynamic.CorrelationIDFromContext(ctx),
		ActorID:       dynamic.ActorIDFromContext(ctx),
		BranchID:      dynamic.BranchIDFromContext(ctx),
	}
	if len(payload) == 0 {
		return ev, nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(payload, &top) != nil {
		return ev, nil // not an object: no aggregate to read
	}
	raw, ok := top["aggregate"]
	if !ok || string(raw) == "null" {
		return ev, nil
	}
	var agg struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(raw, &agg); err != nil {
		return ev, fmt.Errorf("aggregate must be an object {type, id} of strings")
	}
	if agg.Type == "" && agg.ID == "" {
		return ev, nil
	}
	if !validAggregateField(agg.Type) || !validAggregateField(agg.ID) {
		return ev, fmt.Errorf("aggregate.type and aggregate.id must be 1-%d bytes of [A-Za-z0-9_.:-]", maxAggregateField)
	}
	ev.AggregateType, ev.AggregateID = agg.Type, agg.ID
	return ev, nil
}

// flushEmits runs once per invocation, after the guest returned. tx is the
// caller's open transaction (InvokeInTx), when there is one.
func (h *Host) flushEmits(ctx context.Context, tx *gorm.DB, buf *emitBuffer, addonKey, funcName string, out []byte, runErr error) {
	evs := buf.take()
	if len(evs) == 0 {
		return
	}
	if runErr != nil || !invocationCommitted(out) {
		h.logger.Printf("metacore.wasm addon=%s fn=%s discarded %d staged event(s): invocation did not succeed",
			addonKey, funcName, len(evs))
		return
	}
	// The invocation is done; publication must not die with a request ctx.
	pub := h.eventPublisher()
	if pub == nil {
		return
	}
	// A pool handle (Native actions invoke with actx.DB = pool) is not a
	// transaction: treating it as one defers the fan-out to a commit hook that
	// never fires, so the event waited for the relay (~40 s) or was dropped.
	// Without a real tx the publisher persists and publishes on the normal path.
	if !isTx(tx) {
		tx = nil
	}
	pub.Publish(context.WithoutCancel(ctx), tx, evs)
}

// WithEventPublisher replaces the durable publisher that commits a successful
// invocation's staged events. By default the host builds one over its bus and
// DB (kernel_event_outbox, so the outbox relay recovers a crash between persist
// and publish); tests inject their own to simulate that crash. The outbox is
// armed (migrated) here, at wiring time, never on the first emit.
func (h *Host) WithEventPublisher(p *dynamic.OutboxPublisher) *Host {
	h.pubMu.Lock()
	h.pub, h.pubAuto = p, false
	h.pubMu.Unlock()
	p.Arm()
	return h
}

// WithDurableEmits chooses whether guest events are persisted to
// kernel_event_outbox before fan-out (default true when the host has a DB).
// Durable emits are only recovered after a crash if SOMETHING sweeps the outbox:
// dynamic.Service does (RelayOutboxOnce every 30 s) unless it was built with
// DisableEventOutbox. A host that sets DisableEventOutbox, or runs the wasm
// host without a dynamic.Service, must pass false here (events then publish
// straight to the bus after the invocation succeeds, with no table and no
// recovery) or call Service.RelayOutboxOnce itself. Call it before the first
// invocation; it rebuilds the default publisher.
//
// Caveat: with durability off, an InvokeInTx without a commit hook DISCARDS
// the events of the tx (only a log remains); the hook is then mandatory.
func (h *Host) WithDurableEmits(durable bool) *Host {
	h.pubMu.Lock()
	h.durableOff = !durable
	if h.pubAuto || h.pub == nil {
		h.pub, h.pubAuto = nil, false
	}
	h.pubMu.Unlock()
	h.ensurePublisher()
	return h
}

// ensurePublisher (re)builds the default publisher from the current bus / DB /
// durability choice and arms it. Called from WithBus, WithDB and
// WithDurableEmits so the migration runs at wiring, not on the first emit. A
// publisher installed with WithEventPublisher is left alone.
func (h *Host) ensurePublisher() {
	h.pubMu.Lock()
	if h.bus == nil || (h.pub != nil && !h.pubAuto) {
		h.pubMu.Unlock()
		return
	}
	db := h.db
	if h.durableOff {
		db = nil
	}
	p := dynamic.NewOutboxPublisher(db, h.bus)
	h.pub, h.pubAuto = p, true
	h.pubMu.Unlock()
	p.Arm()
}

func (h *Host) eventPublisher() *dynamic.OutboxPublisher {
	h.pubMu.Lock()
	p := h.pub
	h.pubMu.Unlock()
	if p == nil && h.bus != nil {
		h.ensurePublisher()
		h.pubMu.Lock()
		p = h.pub
		h.pubMu.Unlock()
	}
	return p
}

// CommitHook collects the publication of events staged inside a caller's
// transaction until that transaction commits (see dynamic.CommitHook).
type CommitHook = dynamic.CommitHook

// WithCommitHook returns a ctx for Host.InvokeInTx / InvokeForInTx and the hook
// to run after the caller's tx.Commit():
//
//	ctx, hook := wasm.WithCommitHook(ctx)
//	out, err := host.InvokeInTx(ctx, tx, ...)
//	if err == nil { err = tx.Commit().Error }
//	if err != nil { hook.Discard() } else { host.PublishCommitted(ctx, hook) }
//
// Inside a transaction event_emit persists to the outbox in that tx and never
// publishes in line; without calling PublishCommitted the committed rows are
// published by the outbox relay instead (later, but not lost while the outbox
// is armed).
//
// WITHOUT durability (WithDurableEmits(false), or the outbox not armed) and
// InvokeInTx without a hook, the events of the tx are DISCARDED: nothing is
// persisted and nothing publishes, only a log line remains. In that case the
// commit hook is mandatory.
//
// Double publication: if the tx stays open longer than ~10 s, both the relay
// and the hook can publish the same row. The ledger collapses the duplicate,
// but taps and plain subscribers see the event twice (at-least-once).
func WithCommitHook(ctx context.Context) (context.Context, *CommitHook) {
	return dynamic.WithCommitHook(ctx)
}

// PublishCommitted fans out the events staged in a now-committed transaction.
func (h *Host) PublishCommitted(ctx context.Context, hook *CommitHook) { hook.Run(ctx) }

// rebuildAutoPublisher drops a host-built publisher (its bus / db are stale)
// and builds + arms a fresh one, so the outbox migrates when the host is wired.
func (h *Host) rebuildAutoPublisher() {
	h.pubMu.Lock()
	if h.pubAuto {
		h.pub, h.pubAuto = nil, false
	}
	h.pubMu.Unlock()
	h.ensurePublisher()
}
