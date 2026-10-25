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

// stageEmit builds the StagedEvent for one event_emit call, stamping the host
// identity (id, time, causation, actor, branch, correlation) from the call's
// ctx and reading the optional aggregate the guest declared in its payload:
//
//	{"aggregate": {"type": "SalesOrder", "id": "…"}, …}
func stageEmit(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload []byte) dynamic.StagedEvent {
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
	var p struct {
		Aggregate *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"aggregate"`
	}
	if len(payload) > 0 && json.Unmarshal(payload, &p) == nil && p.Aggregate != nil &&
		p.Aggregate.Type != "" && p.Aggregate.ID != "" {
		ev.AggregateType, ev.AggregateID = p.Aggregate.Type, p.Aggregate.ID
	}
	return ev
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
	pub.Publish(context.WithoutCancel(ctx), tx, evs)
}

// WithEventPublisher replaces the durable publisher that commits a successful
// invocation's staged events. By default the host builds one over its bus and
// DB (kernel_event_outbox, so the outbox relay recovers a crash between persist
// and publish); tests inject their own to simulate that crash.
func (h *Host) WithEventPublisher(p *dynamic.OutboxPublisher) *Host {
	h.pubMu.Lock()
	h.pub = p
	h.pubMu.Unlock()
	return h
}

func (h *Host) eventPublisher() *dynamic.OutboxPublisher {
	h.pubMu.Lock()
	defer h.pubMu.Unlock()
	if h.pub == nil && h.bus != nil {
		h.pub = dynamic.NewOutboxPublisher(h.db, h.bus)
	}
	return h.pub
}
