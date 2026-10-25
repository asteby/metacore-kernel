package dynamic

import "context"

// Meta is the host-stamped identity of ONE publication, carried on the ctx
// handed to bus.Publish. It is how the durable layers (the event outbox and
// the wasm event_emit buffer) give a publication a STABLE identity that
// survives a relay re-publish: the ledger derives its idempotency key from
// Meta.ID, so replaying the persisted event after a crash collapses into the
// deliveries already recorded instead of fanning out a second time.
//
// Every field is optional: a publisher that sets none (a plain
// bus.Publish(...)) gets the pre-envelope behaviour.
type Meta struct {
	// ID is the publication id (uuid). Stable across relay re-publishes.
	ID string
	// Version is the event schema major version; 0 means 1.
	Version int
	// OccurredAt is when the event happened, RFC 3339 (UTC).
	OccurredAt string
	// AggregateType / AggregateID name the entity the event is about.
	AggregateType string
	AggregateID   string
	// Sequence is the per-aggregate monotonic counter, 0 = unsequenced.
	Sequence int64
	// CausationID is the id of the event whose delivery caused this one.
	CausationID string
}

type metaKey struct{}

// WithMeta returns ctx carrying m for the subscribers of the next publish.
func WithMeta(ctx context.Context, m Meta) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, metaKey{}, m)
}

// MetaFromContext returns the Meta recorded by WithMeta (zero value if none).
func MetaFromContext(ctx context.Context) Meta {
	if ctx == nil {
		return Meta{}
	}
	m, _ := ctx.Value(metaKey{}).(Meta)
	return m
}

type causationKey struct{}

// WithCausationID records on ctx the id of the event whose delivery is running.
// Anything published from that ctx (a guest event_emit, a data_mutate) names it
// as its causation_id, which is what links a chain of events together.
func WithCausationID(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, causationKey{}, id)
}

// CausationIDFromContext returns the id recorded by WithCausationID, or "".
func CausationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(causationKey{}).(string)
	return s
}
