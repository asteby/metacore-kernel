package events

import (
	"context"

	"github.com/google/uuid"
)

// Estimator is the optional dry-run face of a RoutingHandler: how many
// deliveries it would make for event in orgID, without delivering. ok=false
// means "cannot tell".
type Estimator func(ctx context.Context, orgID uuid.UUID, event string) (n int, ok bool)

// CheckEmit runs the event:emit capability check for addonKey without
// publishing anything. A durable emitter (wasm event_emit) uses it to refuse a
// forbidden event at call time, while the publication itself is deferred until
// the invocation commits.
func (b *Bus) CheckEmit(addonKey, event string) error {
	return b.check(addonKey, "event:emit", event)
}

// EstimateSubscribers estimates the fan-out of event in orgID for emitters that
// publish later. It is a dry run, not a promise: observers registered with
// SubscribeTap (audit) count 0; a plain subscriber counts 1; a router counts
// what its Estimator says. When any matching router has no Estimator (or its
// Estimator cannot tell) the answer is -1 ("unknown") rather than a number that
// would mislead the emitter.
func (b *Bus) EstimateSubscribers(ctx context.Context, orgID uuid.UUID, event string) int {
	b.mu.RLock()
	var matched []subscription
	for pattern, subs := range b.subs {
		if eventMatches(pattern, event) {
			matched = append(matched, subs...)
		}
	}
	b.mu.RUnlock()
	n := 0
	for _, s := range matched {
		switch {
		case s.tap:
		case s.est != nil:
			c, ok := s.est(ctx, orgID, event)
			if !ok {
				return -1
			}
			n += c
		case s.isRouter:
			return -1
		default:
			n++
		}
	}
	return n
}
