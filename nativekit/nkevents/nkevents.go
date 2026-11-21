// Package nkevents is EmitTx for native modules: it ties a nativekit.Op to the
// kernel's existing transactional event outbox (dynamic.OutboxPublisher), so a
// module publishes a domain event atomically with its data.
//
//	em := nkevents.New(dynamic.NewOutboxPublisher(db, bus), "my_addon")
//	em.Arm() // at boot: migrates kernel_event_outbox
//
//	err := nativekit.Run(ctx, db, func(op *nativekit.Op) error {
//	    // ... module writes through op.Tx() ...
//	    em.EmitTx(op, nkevents.Event{Org: org, Name: "my_addon.invoice.posted",
//	        Payload: map[string]any{"id": id}, AggregateType: "invoice", AggregateID: id})
//	    return nil
//	})
//
// Semantics (at-least-once, ordered per aggregate):
//   - the outbox row (and its per-aggregate sequence bump) is written in the
//     operation's transaction: a rollback removes it and nothing is delivered;
//   - fan-out to the bus runs AFTER commit through the Op's AfterCommit hook;
//   - if the process dies between commit and fan-out, the outbox relay
//     (dynamic.Service.RelayOutboxOnce / StartOutboxRelay) republishes the row;
//     consumers dedupe on the event id the dispatch ledger already keys on;
//   - delivery order follows the aggregate sequence, so use a stable
//     AggregateType/AggregateID for events that must stay ordered.
package nkevents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/nativekit"
)

// Event is one domain event to publish.
type Event struct {
	Org     uuid.UUID
	Name    string
	Payload any // JSON-encodable
	// AggregateType/AggregateID give the event its per-aggregate order.
	AggregateType, AggregateID string
	Actor, Branch              string
	CausationID, CorrelationID string
}

// Emitter publishes events of one addon through an outbox publisher.
type Emitter struct {
	pub   *dynamic.OutboxPublisher
	addon string
}

// New binds a publisher to the emitting addon key (the bus capability subject).
func New(pub *dynamic.OutboxPublisher, addonKey string) *Emitter {
	return &Emitter{pub: pub, addon: addonKey}
}

// Arm migrates the outbox tables; call it at boot, never on the hot path.
func (e *Emitter) Arm() bool { return e.pub.Arm() }

// EmitTx stages evs in op's transaction and schedules their fan-out after
// commit. It returns an error only for an unencodable payload or an empty
// name, before anything is written.
func (e *Emitter) EmitTx(op *nativekit.Op, evs ...Event) error {
	staged := make([]dynamic.StagedEvent, 0, len(evs))
	for _, ev := range evs {
		if ev.Name == "" {
			return nativekit.E(nativekit.CodeInvalid, "event name is required")
		}
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			return nativekit.Wrap(nativekit.CodeInvalid, fmt.Sprintf("event %s payload is not JSON-encodable", ev.Name), err)
		}
		staged = append(staged, dynamic.StagedEvent{
			AddonKey: e.addon, Name: ev.Name, OrgID: ev.Org, Payload: raw, Version: 1,
			AggregateType: ev.AggregateType, AggregateID: ev.AggregateID, OccurredAt: time.Now().UTC(),
			ActorID: ev.Actor, BranchID: ev.Branch, CausationID: ev.CausationID, CorrelationID: ev.CorrelationID,
		})
	}
	if len(staged) == 0 {
		return nil
	}
	ctx, hook := dynamic.WithCommitHook(op.Context())
	e.pub.Publish(ctx, op.Tx(), staged)
	op.AfterCommit(func(c context.Context) { hook.Run(c) })
	op.OnRollback(func(context.Context) { hook.Discard() })
	return nil
}
