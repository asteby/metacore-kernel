package dynamic

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"github.com/asteby/metacore-kernel/provenance"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// CanonicalEvent is the payload shape published on the events.Bus for every
// CRUD mutation routed through dynamic.Service. The JSON-equivalent shape is
// `{id, model, action, addon_key, actor_id?, correlation_id?, before?, after?}`:
//
//   - `created` carries `id`, `model`, `action`, `addon_key`, `actor_id`,
//     `correlation_id` + `after` (the fresh row, including server-generated
//     fields like `created_at`).
//   - `updated` carries all envelope fields + `before` (snapshot loaded before
//     the input merge) and `after` (the saved row after Save).
//   - `deleted` carries all envelope fields + `before` (the snapshot just
//     before Delete). A missing `before` on `deleted` means the row was already
//     gone or out of tenant scope when we tried to read it.
//
// Subscribers receive the value untyped (events.Handler signature is
// `func(ctx, orgID, payload any)`) — they should type-assert
// `*dynamic.CanonicalEvent` for in-process delivery and rely on the JSON tags
// when forwarding elsewhere.
//
// The ID, Before, and After fields are preserved with the same names and types
// for backward-compatibility with existing subscribers.
type CanonicalEvent struct {
	ID string `json:"id"`
	// OccurrenceID identifies this PUBLICATION, not the mutated row. Two
	// updates of the same row are two occurrences; a re-publish of one
	// occurrence (outbox relay) reuses the persisted payload bytes and
	// therefore keeps its id, which is what lets the dispatch ledger dedup
	// re-delivery without swallowing every update after the first.
	OccurrenceID string `json:"occurrence_id,omitempty"`
	Model        string `json:"model"`
	Action       string `json:"action"` // created|updated|deleted
	AddonKey     string `json:"addon_key"`
	ActorID      string `json:"actor_id,omitempty"`
	// BranchID is the branch the event belongs to: the row's own branch_id
	// when it has one (the document's branch), else the caller's active
	// branch (WithBranchID). The dispatcher re-attaches it to the delivery
	// ctx so a subscriber's creates are born in the same branch.
	BranchID      string         `json:"branch_id,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Before        map[string]any `json:"before,omitempty"`
	After         map[string]any `json:"after,omitempty"`
	// Reason is the operator's stated motive for a destructive operation
	// (delete, or an action listed in Model.reason_required — PER-4). Empty on
	// every other event.
	Reason string `json:"reason,omitempty"`
	// Provenance is who made the change and how (person, agent, system,
	// integration, or a person with an agent), copied from the request/job
	// context (provenance.WithContext). Nil when the caller set none.
	Provenance *provenance.Provenance `json:"provenance,omitempty"`
}

// correlationIDKey is an unexported typed key used to store a correlation ID
// in a context.Context. Using a private struct type (instead of a plain string)
// avoids collisions with keys from other packages.
type correlationIDKey struct{}

// WithCorrelationID returns a child context carrying the given correlation ID.
// log.HTTPMiddleware and log.FiberMiddleware stamp the request's X-Request-ID
// here. A host that invokes wasm outside those middlewares should do the same
// before Invoke so error envelopes and canonical events share one id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, id)
}

// CorrelationIDFromContext extracts the correlation ID previously stored by
// WithCorrelationID. It returns an empty string when no ID was set.
func CorrelationIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationIDKey{}).(string)
	return v
}

// actorIDKey is the unexported typed key for the acting user's id. Mirrors
// correlationIDKey: the dispatcher re-attaches the originating event's actor
// to the (detached) delivery context so downstream host imports — data_mutate
// above all — can stamp their own canonical events with the human that caused
// the chain, not an anonymous system actor.
type actorIDKey struct{}

// WithActorID returns a child context carrying the acting user's id. An empty
// id returns ctx unchanged.
func WithActorID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, actorIDKey{}, id)
}

// ActorIDFromContext extracts the actor id previously stored by WithActorID.
// It returns an empty string when no id was set.
func ActorIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(actorIDKey{}).(string)
	return v
}

// beforeRowKey is the unexported typed key for the row a mutation replaced.
type beforeRowKey struct{}

// WithBeforeRow returns a child context carrying the row an UPDATE replaced
// (nil/empty returns ctx unchanged). The wasm data_mutate / data_batch paths set
// it before the declarative compute pass, whose signature has no `before`
// parameter, so cross-record rules can tell an untouched relation from a
// changed one exactly as they do on the dynamic.Service path.
func WithBeforeRow(ctx context.Context, before map[string]any) context.Context {
	if len(before) == 0 {
		return ctx
	}
	return context.WithValue(ctx, beforeRowKey{}, before)
}

// BeforeRowFromContext returns the row stored by WithBeforeRow, nil when none.
func BeforeRowFromContext(ctx context.Context) map[string]any {
	v, _ := ctx.Value(beforeRowKey{}).(map[string]any)
	return v
}

// branchIDKey is the unexported typed key for the acting user's active branch
// (the sucursal the host's branch switcher has selected). It rides next to the
// actor so a create issued from a guest (data_mutate / data_batch) lands in the
// same branch a plain CRUD create would, instead of a document with no branch
// that breaks per-branch reports, stock and cash.
type branchIDKey struct{}

// WithBranchID returns a child context carrying the acting user's active branch
// id. An empty id returns ctx unchanged.
func WithBranchID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, branchIDKey{}, id)
}

// BranchIDFromContext extracts the branch id previously stored by WithBranchID.
// It returns an empty string when no branch was set.
func BranchIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(branchIDKey{}).(string)
	return v
}

// EventBranchID resolves the branch a canonical event carries: the mutated
// row's own branch_id (after, else before) — the document's branch wins over
// whoever touched it — else the caller's active branch on ctx. It returns ""
// when none of them is a uuid.
func EventBranchID(ctx context.Context, before, after map[string]any) string {
	for _, row := range []map[string]any{after, before} {
		if id := uuidString(row["branch_id"]); id != "" {
			return id
		}
	}
	return uuidString(BranchIDFromContext(ctx))
}

// uuidString renders a branch_id value (string, uuid.UUID, *uuid.UUID, []byte)
// as a canonical uuid string, "" for nil, blank, the zero uuid or non-uuids.
func uuidString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case *uuid.UUID:
		if t == nil {
			return ""
		}
		v = *t
	case *string:
		if t == nil {
			return ""
		}
		v = *t
	case []byte:
		v = string(t)
	}
	id, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(v)))
	if err != nil || id == uuid.Nil {
		return ""
	}
	return id.String()
}

// publishCanonical builds the event name `<addonKey>.<model>.<action>` and
// fans it out through the Bus. It is a no-op when no Bus was wired, keeping
// pre-event apps unchanged. The producer addonKey passed to Bus.Publish is
// the same one used to namespace the event — the kernel itself bypasses the
// `event:emit` capability check (`events/events.go`), so models with no addon
// owner publish trusted under "kernel".
//
// Errors from the Bus (capability denial, etc.) are intentionally swallowed:
// the DB mutation has already committed, the Bus already logs the failure,
// and we keep the same "post-mutation side-effects must not roll back the
// data" policy as the AfterCreate/Update/Delete hooks.
func (s *Service) publishCanonical(ctx context.Context, model, action string, user modelbase.AuthUser, id string, before, after map[string]any) {
	if s.bus == nil {
		return
	}
	addonKey := "kernel"
	if s.addonKeyForModel != nil {
		if k := s.addonKeyForModel(ctx, model); k != "" {
			addonKey = k
		}
	}
	// Canonicalize the model segment to the manifest ModelKey. Create/Update/
	// Delete may be called with the table/route name (the action-create path
	// passes "transfers"); subscribers register on the ModelKey ("Transfer"),
	// per the contract `<addon>.<ModelKey>.<action>`. Without this the names
	// diverge and the subscribed handler never fires.
	modelKey := model
	if s.modelKeyForModel != nil {
		if mk := s.modelKeyForModel(ctx, model); mk != "" {
			modelKey = mk
		}
	}
	s.rememberModelAlias(model, modelKey)
	payload := &CanonicalEvent{
		ID:            id,
		OccurrenceID:  uuid.NewString(),
		Model:         modelKey,
		Action:        action,
		AddonKey:      addonKey,
		ActorID:       user.GetID().String(),
		BranchID:      EventBranchID(withUserBranch(ctx, user), before, after),
		CorrelationID: CorrelationIDFromContext(ctx),
		Reason:        ReasonFromContext(ctx),
		Provenance:    provenanceFromContext(ctx),
		Before:        before,
		After:         after,
	}
	event := fmt.Sprintf("%s.%s.%s", addonKey, modelKey, action)
	if len(event) > MaxEventNameLen {
		// Permanent: no store can record this name, so retrying would burn the
		// relay's attempts for nothing. Reject it before persisting.
		log.Printf("level=error msg=kernel.outbox.rejected reason=event_name_too_long len=%d max=%d event=%.80s: %v",
			len(event), MaxEventNameLen, event, ErrEventNameTooLong)
		return
	}

	// Transactional-outbox durability (see outbox.go): persist the intent
	// BEFORE fan-out so a crash or bus failure between the committed data
	// write and the publish can no longer lose the event — the relay
	// re-publishes anything that never got stamped published_at.
	var outboxID uuid.UUID
	pubCtx := ctx
	occurred := time.Now().UTC()
	meta := Meta{
		ID:            payload.OccurrenceID,
		Version:       1,
		OccurredAt:    occurred.Format(time.RFC3339Nano),
		AggregateType: modelKey,
		AggregateID:   id,
		CausationID:   CausationIDFromContext(ctx),
	}
	if s.outboxEnabled {
		if raw, merr := json.Marshal(payload); merr == nil {
			row := &EventOutbox{
				ID:             uuid.New(),
				OrganizationID: user.GetOrganizationID(),
				AddonKey:       addonKey,
				EventName:      event,
				Payload:        string(raw),
				CreatedAt:      occurred,
				EventID:        payload.OccurrenceID,
				Version:        1,
				OccurredAt:     &occurred,
				AggregateType:  modelKey,
				AggregateID:    id,
				CausationID:    meta.CausationID,
				CorrelationID:  payload.CorrelationID,
				ActorID:        payload.ActorID,
				BranchID:       payload.BranchID,
			}
			if id != "" {
				// Best effort: an unsequenced event (0) is still delivered.
				if seq, serr := nextAggregateSequence(ctx, s.db, user.GetOrganizationID(), modelKey, id); serr == nil {
					row.Sequence = seq
				}
			}
			meta.Sequence = row.Sequence
			if s.persistOutbox(ctx, row) {
				outboxID = row.ID
			}
		}
	}
	pubCtx = WithMeta(ctx, meta)

	perr := s.bus.Publish(pubCtx, addonKey, event, user.GetOrganizationID(), payload)
	if perr == nil && outboxID != uuid.Nil {
		s.markOutboxPublished(ctx, outboxID)
	} else if perr != nil && outboxID != uuid.Nil {
		// A durable router failed to record the event: the row stays
		// unpublished and the outbox relay retries it.
		if siblingsRan(perr) && s.db != nil {
			// The plain subscribers already ran in this attempt: the relay
			// must retry the durable routers only (no duplicate side effects).
			markOutboxFannedOut(ctx, s.db, outboxID)
		}
		log.Printf("dynamic: event %s not recorded by the durable bus (outbox relay will retry): %v", event, perr)
	}
}

// withUserBranch adds the user's active branch (the optional GetBranchID the
// sequence scope and ExecAction honour) to ctx when ctx carries none.
func withUserBranch(ctx context.Context, user any) context.Context {
	if BranchIDFromContext(ctx) != "" {
		return ctx
	}
	if b, ok := user.(interface{ GetBranchID() uuid.UUID }); ok && b.GetBranchID() != uuid.Nil {
		return WithBranchID(ctx, b.GetBranchID().String())
	}
	return ctx
}

// provenanceFromContext copies the request/job provenance for the event.
func provenanceFromContext(ctx context.Context) *provenance.Provenance {
	if p, ok := provenance.FromContext(ctx); ok {
		return &p
	}
	return nil
}
