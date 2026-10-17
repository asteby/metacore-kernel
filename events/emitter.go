package events

import "context"

// emitterKey is the context key under which the bus records who published the
// event being delivered.
type emitterKey struct{}

// WithEmitter returns ctx carrying emitter as the publisher of the event in
// flight. Bus.PublishWithCount calls it on every publish, overwriting any
// emitter already in ctx, so a subscriber always sees the key the bus was
// called with: an addon key for a guest's event_emit, "kernel" for
// host-originated events and the owner addon key for canonical CRUD events the
// host publishes on an addon's behalf.
//
// Hosts normally never call it directly; it is exported so a handler invoked
// outside the bus (tests, replays) can reproduce the same context.
func WithEmitter(ctx context.Context, emitter string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, emitterKey{}, emitter)
}

// EmitterFromContext returns the publisher recorded by WithEmitter, or "" when
// the context did not come through Bus.PublishWithCount.
//
// The value is host-authored: it is the addonKey argument of Publish, never
// something read from the payload, so a guest cannot claim another addon's
// identity by writing an "emitter" field into its payload.
func EmitterFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(emitterKey{}).(string)
	return s
}
