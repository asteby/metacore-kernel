package events

// CheckEmit runs the event:emit capability check for addonKey without
// publishing anything. A durable emitter (wasm event_emit) uses it to refuse a
// forbidden event at call time, while the publication itself is deferred until
// the invocation commits.
func (b *Bus) CheckEmit(addonKey, event string) error {
	return b.check(addonKey, "event:emit", event)
}

// MatchCount reports how many subscriptions currently match event. It is an
// estimate of the fan-out size for emitters that publish later (a RoutingHandler
// may deliver to more or fewer subscribers than the one tap that matched).
func (b *Bus) MatchCount(event string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for pattern, subs := range b.subs {
		if eventMatches(pattern, event) {
			n += len(subs)
		}
	}
	return n
}
