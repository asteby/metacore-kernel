// Package events provides the in-process Bus the kernel exposes to addons for
// fan-out of domain events (ticket.created, order.paid, ...). It is an
// intentionally minimal implementation: a single process, no broker, no
// persistence. F3 will swap the transport underneath while keeping the Publish
// / Subscribe surface stable.
//
// Capability model
//
//	Publish:    caller must hold event:emit      for the event name
//	Subscribe:  caller must hold event:subscribe for the event pattern
//
// Both checks run through a security.Enforcer so operators can flip between
// shadow (log only) and enforce (error) globally without code changes.
//
// # Wildcard subscription
//
// A subscribe pattern ending in ".*" matches any event sharing the prefix:
// "ticket.*" matches "ticket.created", "ticket.resolved", but not
// "tickets.bulk". This mirrors the glob semantics used by the capability
// resolver so authors only learn one rule.
package events

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// Handler is the callback invoked on a matching event. Handlers run
// synchronously inside Publish — each handler returning an error is logged
// but does not short-circuit delivery to siblings. Long-running handlers
// should hand work off to their own goroutine.
type Handler func(ctx context.Context, orgID uuid.UUID, payload any) error

// EventHandler is the name-aware sibling of Handler: it receives the NAME of
// the event that matched in addition to the payload. Handler cannot express
// this — it only ever sees the payload — which is why routing layers such as
// the dispatcher had to reconstruct a name from the payload's shape (and could
// therefore only route canonical CRUD envelopes). Subscribe/Handler stay as
// they are for source compatibility; new name-dependent subscribers should use
// SubscribeEvent.
type EventHandler func(ctx context.Context, orgID uuid.UUID, event string, payload any) error

// RoutingHandler is a subscriber that is itself a fan-out layer: rather than
// being one subscriber, it forwards the event to N downstream handlers and
// reports how many it actually accepted. The bus adds that number — not 1 — to
// the count PublishWithCount returns, so an emitter learns how many deliveries
// its event really produced instead of how many taps happened to be attached.
// A router that drops the event returns 0.
type RoutingHandler func(ctx context.Context, orgID uuid.UUID, event string, payload any) (int, error)

// subscription binds a pattern + handler to an addon (for capability checks
// and bulk Unsubscribe by addon key).
type subscription struct {
	AddonKey string
	Pattern  string
	// fn is the normalised callback: every Subscribe* variant is adapted to
	// this one shape so Publish has a single call path.
	fn RoutingHandler
}

// Bus is the thread-safe fan-out registry. The zero value is not usable —
// construct with NewBus.
type Bus struct {
	enforcer *security.Enforcer
	logger   *log.Logger

	mu   sync.RWMutex
	subs map[string][]subscription // keyed by pattern for slightly cheaper match scans
}

// NewBus returns a Bus wired to an Enforcer. The enforcer may be nil, in
// which case every capability check is skipped (useful in tests and during
// kernel bring-up before the enforcer is constructed).
func NewBus(enforcer *security.Enforcer) *Bus {
	return &Bus{
		enforcer: enforcer,
		logger:   log.Default(),
		subs:     make(map[string][]subscription),
	}
}

// WithLogger replaces the default logger (useful in tests).
func (b *Bus) WithLogger(l *log.Logger) *Bus {
	if l != nil {
		b.logger = l
	}
	return b
}

// Subscribe registers handler for every event matching pattern. The pattern
// is either a literal event name ("ticket.created") or a wildcard suffix
// ("ticket.*"). The addonKey is used for the event:subscribe capability
// check; pass "kernel" when the host itself is subscribing.
func (b *Bus) Subscribe(addonKey, eventPattern string, h Handler) error {
	if eventPattern == "" {
		return fmt.Errorf("events: empty pattern")
	}
	if h == nil {
		return fmt.Errorf("events: nil handler")
	}
	return b.SubscribeRouting(addonKey, eventPattern, func(ctx context.Context, orgID uuid.UUID, _ string, payload any) (int, error) {
		return 1, h(ctx, orgID, payload)
	})
}

// SubscribeEvent is Subscribe for a name-aware EventHandler. Semantics are
// otherwise identical (same pattern rule, same capability check, same
// logged-and-swallowed error handling); it counts as one subscriber.
func (b *Bus) SubscribeEvent(addonKey, eventPattern string, h EventHandler) error {
	if h == nil {
		return fmt.Errorf("events: nil handler")
	}
	return b.SubscribeRouting(addonKey, eventPattern, func(ctx context.Context, orgID uuid.UUID, event string, payload any) (int, error) {
		return 1, h(ctx, orgID, event, payload)
	})
}

// SubscribeRouting registers a RoutingHandler — a subscriber whose reported
// delivery count, not its mere presence, feeds the PublishWithCount total.
// Used by the dispatcher, which is a router for every addon subscription of
// the org rather than a subscriber in its own right.
func (b *Bus) SubscribeRouting(addonKey, eventPattern string, h RoutingHandler) error {
	if eventPattern == "" {
		return fmt.Errorf("events: empty pattern")
	}
	if h == nil {
		return fmt.Errorf("events: nil handler")
	}
	if err := b.check(addonKey, "event:subscribe", eventPattern); err != nil {
		return err
	}
	b.mu.Lock()
	b.subs[eventPattern] = append(b.subs[eventPattern], subscription{
		AddonKey: addonKey,
		Pattern:  eventPattern,
		fn:       h,
	})
	b.mu.Unlock()
	b.logger.Printf("metacore.events subscribe addon=%s pattern=%s", addonKey, eventPattern)
	return nil
}

// Publish delivers event+payload to every subscriber whose pattern matches.
// The addonKey identifies the producer ("kernel" for host-originated events);
// it drives the event:emit capability check. Handler errors are logged and
// otherwise swallowed so one faulty subscriber cannot block siblings.
//
// Publish is a thin wrapper over PublishWithCount kept for source compat —
// callers that need the matched-subscriber count (e.g. the wasm `event_emit`
// envelope, see docs/wasm-abi.md § 12.4) should call PublishWithCount
// directly.
func (b *Bus) Publish(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) error {
	_, err := b.PublishWithCount(ctx, addonKey, event, orgID, payload)
	return err
}

// PublishWithCount is the count-returning sibling of Publish. It mirrors the
// Publish semantics 1:1 (capability check → match scan → synchronous fan-out)
// and additionally returns the number of DELIVERIES the event produced. A
// plain Handler/EventHandler subscriber counts as 1 when it runs; a
// RoutingHandler (see SubscribeRouting) contributes the number of downstream
// deliveries it reports, which is 0 when it drops the event. The count is
// therefore a fan-out size, not a success count — handler errors are still
// logged-and-swallowed, so `(3, nil)` means three deliveries were made (one or
// more may have errored internally). What it will NOT do is report 1 because
// some wildcard tap matched and then discarded the event.
//
// On capability denial (enforce mode) or input error the count is `0` and
// the error is non-nil. Capability denial in `ModeShadow` returns
// `(matched, nil)` mirroring the bus's pre-existing behaviour.
func (b *Bus) PublishWithCount(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) (int, error) {
	if event == "" {
		return 0, fmt.Errorf("events: empty event name")
	}
	if err := b.check(addonKey, "event:emit", event); err != nil {
		return 0, err
	}

	b.mu.RLock()
	matched := make([]subscription, 0, 4)
	for pattern, subs := range b.subs {
		if eventMatches(pattern, event) {
			matched = append(matched, subs...)
		}
	}
	b.mu.RUnlock()

	b.logger.Printf("metacore.events publish org=%s event=%s caller=%s matched=%d",
		orgID, event, addonKey, len(matched))

	delivered := 0
	for _, s := range matched {
		n, err := s.fn(ctx, orgID, event, payload)
		if err != nil {
			b.logger.Printf("metacore.events handler_error addon=%s pattern=%s event=%s err=%v",
				s.AddonKey, s.Pattern, event, err)
		}
		if n > 0 {
			delivered += n
		}
	}
	return delivered, nil
}

// Unsubscribe removes every subscription registered under addonKey. It is the
// teardown path called when an addon is uninstalled / disabled so handlers
// referencing its code do not fire against a stale runtime.
func (b *Bus) Unsubscribe(addonKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	removed := 0
	for pat, list := range b.subs {
		kept := list[:0]
		for _, s := range list {
			if s.AddonKey == addonKey {
				removed++
				continue
			}
			kept = append(kept, s)
		}
		if len(kept) == 0 {
			delete(b.subs, pat)
		} else {
			b.subs[pat] = kept
		}
	}
	if removed > 0 {
		b.logger.Printf("metacore.events unsubscribe addon=%s removed=%d", addonKey, removed)
	}
}

// check runs the capability check via the injected Enforcer. The kernel
// itself (addonKey == "kernel") is trusted and skips the check — otherwise
// every addon origin must be registered with the enforcer.
func (b *Bus) check(addonKey, kind, target string) error {
	if b.enforcer == nil || addonKey == "kernel" {
		return nil
	}
	return b.enforcer.CheckCapability(addonKey, kind, target)
}

// eventMatches implements the wildcard rule described in the package docs.
// It is intentionally tiny — no regex compilation — because the hot path
// runs under the Publish read-lock. Only trailing ".*" wildcards are
// honoured, matching the capability resolver.
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
