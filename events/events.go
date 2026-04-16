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
// Wildcard subscription
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

// subscription binds a pattern + handler to an addon (for capability checks
// and bulk Unsubscribe by addon key).
type subscription struct {
	AddonKey string
	Pattern  string
	Handler  Handler
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
	if err := b.check(addonKey, "event:subscribe", eventPattern); err != nil {
		return err
	}
	b.mu.Lock()
	b.subs[eventPattern] = append(b.subs[eventPattern], subscription{
		AddonKey: addonKey,
		Pattern:  eventPattern,
		Handler:  h,
	})
	b.mu.Unlock()
	b.logger.Printf("metacore.events subscribe addon=%s pattern=%s", addonKey, eventPattern)
	return nil
}

// Publish delivers event+payload to every subscriber whose pattern matches.
// The addonKey identifies the producer ("kernel" for host-originated events);
// it drives the event:emit capability check. Handler errors are logged and
// otherwise swallowed so one faulty subscriber cannot block siblings.
func (b *Bus) Publish(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) error {
	if event == "" {
		return fmt.Errorf("events: empty event name")
	}
	if err := b.check(addonKey, "event:emit", event); err != nil {
		return err
	}

	b.mu.RLock()
	matched := make([]subscription, 0, 4)
	for pattern, subs := range b.subs {
		if eventMatches(pattern, event) {
			matched = append(matched, subs...)
		}
	}
	b.mu.RUnlock()

	b.logger.Printf("metacore.events publish org=%s event=%s caller=%s subscribers=%d",
		orgID, event, addonKey, len(matched))

	for _, s := range matched {
		if err := s.Handler(ctx, orgID, payload); err != nil {
			b.logger.Printf("metacore.events handler_error addon=%s pattern=%s event=%s err=%v",
				s.AddonKey, s.Pattern, event, err)
		}
	}
	return nil
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
