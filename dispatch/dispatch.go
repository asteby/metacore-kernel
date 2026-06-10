// Package dispatch is the event-subscription dispatcher: the layer that routes
// the kernel's canonical bus events to the handlers (wasm / compiled) that
// installed addons declare via `contributions.subscriptions` in their v3
// manifest.
//
// # Why it exists
//
// dynamic.publishCanonical fans every CRUD mutation out on the events.Bus as
// `<addonKey>.<model>.<action>`, and audit.Wire already consumes that stream
// for the activity log. But the *addons'* declared subscriptions were, until
// now, purely descriptive: an addon could say "on inventory.product.updated,
// run my wasm handler `recompute_stock`" and the kernel never delivered it.
// Hosts papered over the gap with bespoke compiled-in glue (e.g. ops'
// sales_stock_effects.go). This package is the generic replacement.
//
// Shape
//
//	cancel, err := dispatch.Wire(bus, invoker, db, provider, opts...)
//	if err != nil { return err }
//	defer cancel()
//
// Wire subscribes ONE trusted "kernel" wildcard handler to the bus (mirroring
// audit.Wire). On each CanonicalEvent it:
//
//  1. asks the SubscriptionProvider for the installed+enabled addons of THAT
//     org and their declared subscriptions;
//  2. matches each subscription's `event` against the fired event name using
//     the same wildcard rule as events.Bus (exact + trailing ".*");
//  3. for each match, re-checks the addon's `event:subscribe` capability (the
//     dispatcher subscribes as trusted "kernel" for fan-out, but every actual
//     delivery is gated on the *addon's* own capability — never the trusted
//     key, see SECURITY note below);
//  4. records a deterministic `event_deliveries` row and dispatches the
//     handler ASYNCHRONOUSLY on a worker goroutine so a slow subscriber never
//     blocks the CRUD mutation that produced the event;
//  5. retries a failed delivery up to MaxAttempts, then marks it dead.
//
// SECURITY: the bus capability check bypasses the Enforcer for the literal
// "kernel" key (events/events.go). The dispatcher subscribes under "kernel" so
// it sees every event even in ModeEnforce, but it MUST NOT dispatch an addon's
// handler under that trust. Every delivery is therefore gated by
// CapabilityChecker.CanDeliver(addonKey, event) BEFORE invocation, and the
// wasm db_exec / event_emit imports the handler calls remain gated by the
// addon's own compiled Capabilities exactly as they are for action handlers.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// subscriberKey is the trusted bus key the dispatcher registers its wildcard
// handler under. It MUST be exactly "kernel" for the same reason audit's is:
// events.Bus bypasses the Enforcer only for that literal key, so the
// dispatcher keeps seeing events when a host runs the bus in ModeEnforce.
// Per-addon authorization happens separately (CapabilityChecker), so this
// trusted subscription is purely a fan-out tap, not an authorization grant.
const subscriberKey = "kernel"

// handlerTypeWasm / handlerTypeCompiled are the v3 `handler.type` discriminator
// values the dispatcher routes. "webhook" is intentionally unhandled here — the
// dispatcher is the in-process tier; a webhook fan-out belongs to a future
// transport and would log an unsupported-tier warning.
const (
	handlerTypeWasm     = "wasm"
	handlerTypeCompiled = "compiled"
)

// Subscription is the dispatcher's view of one declared subscription on an
// installed addon. It is the projection a SubscriptionProvider returns — the
// host materialises it from the stored v3 manifest of an installed+enabled
// addon. It deliberately mirrors v3.Subscription + the addon's installation
// identity (needed to invoke the right per-installation wasm module).
type Subscription struct {
	// AddonKey is the owning addon (namespacing + capability subject).
	AddonKey string
	// Installation is the per-(addon,org) installation id the wasm runtime
	// keys its module instance by. Required for HandlerType=="wasm".
	Installation uuid.UUID
	// Event is the subscribed pattern: an exact event name
	// ("inventory.product.updated") or a trailing wildcard ("inventory.*").
	Event string
	// HandlerType is the v3 handler discriminator: "wasm" | "compiled".
	HandlerType string
	// Function is the wasm export name or the compiled symbol to invoke.
	Function string
	// Settings is the installation's stored settings, threaded to the wasm
	// invocation (env_get). Optional.
	Settings map[string]string
}

// SubscriptionProvider yields the subscriptions to evaluate for a given org.
// The host wires it over its own installation store — exactly the seam
// dynamic.Service.addonKeyForModel uses. The dispatcher NEVER caches the
// result, so a freshly installed/enabled addon is picked up on its next event
// with no dispatcher restart.
//
// Implementations MUST return only subscriptions of addons that are BOTH
// installed AND enabled for orgID. Returning a disabled addon's subscription
// would deliver to a torn-down module. An error is logged and that event's
// dispatch is skipped (the source mutation already committed).
type SubscriptionProvider interface {
	SubscriptionsForOrg(ctx context.Context, orgID uuid.UUID) ([]Subscription, error)
}

// SubscriptionProviderFunc adapts a plain function to SubscriptionProvider.
type SubscriptionProviderFunc func(ctx context.Context, orgID uuid.UUID) ([]Subscription, error)

// SubscriptionsForOrg satisfies SubscriptionProvider.
func (f SubscriptionProviderFunc) SubscriptionsForOrg(ctx context.Context, orgID uuid.UUID) ([]Subscription, error) {
	return f(ctx, orgID)
}

// WasmInvoker is the seam to the wasm runtime. *wasm.Host satisfies it via its
// InvokeFor method; the dispatch package does NOT import runtime/wasm so it
// stays free of that package's CGO (libpg_query) weight and remains a clean
// leaf. The host wires the adapter in one line (see WasmInvokerFunc).
//
// Contract: InvokeFor compiles+instantiates the addon's module for
// (addonKey, installation), invokes `function` with payload, and returns the
// guest's result bytes. A non-nil error means the invocation itself failed
// (module missing, trap, timeout) and the delivery should be retried. A wasm
// guest that returns a `{success:false}` envelope is NOT an invocation error —
// the dispatcher treats a clean return as delivered (the guest owns its own
// error semantics, same as action handlers).
type WasmInvoker interface {
	InvokeFor(ctx context.Context, orgID, installation uuid.UUID, addonKey, function string, payload []byte, settings map[string]string) ([]byte, error)
}

// WasmInvokerFunc adapts a plain function (e.g. host.Host.InvokeWASMFor) to
// WasmInvoker.
type WasmInvokerFunc func(ctx context.Context, orgID, installation uuid.UUID, addonKey, function string, payload []byte, settings map[string]string) ([]byte, error)

// InvokeFor satisfies WasmInvoker.
func (f WasmInvokerFunc) InvokeFor(ctx context.Context, orgID, installation uuid.UUID, addonKey, function string, payload []byte, settings map[string]string) ([]byte, error) {
	return f(ctx, orgID, installation, addonKey, function, payload, settings)
}

// CompiledHandler is one first-party, compiled-in subscription handler. The
// hot-path tier: a handler the host links into its own binary rather than
// shipping as wasm. It receives the same JSON event payload the wasm tier gets.
type CompiledHandler interface {
	Handle(ctx context.Context, orgID uuid.UUID, payload []byte) error
}

// CompiledHandlerFunc adapts a plain function to CompiledHandler.
type CompiledHandlerFunc func(ctx context.Context, orgID uuid.UUID, payload []byte) error

// Handle satisfies CompiledHandler.
func (f CompiledHandlerFunc) Handle(ctx context.Context, orgID uuid.UUID, payload []byte) error {
	return f(ctx, orgID, payload)
}

// CompiledRegistry resolves a compiled handler symbol to its implementation.
// It is the in-process registry the plan keeps as an interface for the future
// hot path; a nil registry means the compiled tier is inert (a "compiled"
// subscription with no registry is logged once and skipped, never an error).
type CompiledRegistry interface {
	// Lookup returns the handler for symbol and whether it is registered.
	Lookup(function string) (CompiledHandler, bool)
}

// MapCompiledRegistry is a trivial map-backed CompiledRegistry for hosts that
// register a handful of first-party handlers at boot.
type MapCompiledRegistry map[string]CompiledHandler

// Lookup satisfies CompiledRegistry.
func (m MapCompiledRegistry) Lookup(function string) (CompiledHandler, bool) {
	h, ok := m[function]
	return h, ok
}

// CapabilityChecker authorizes a delivery. The dispatcher calls
// CanDeliver(addonKey, event) before EVERY invocation. A nil checker (the
// default when no enforcer is wired) permits all deliveries — acceptable in
// tests and trusted single-tenant embeds, but production hosts SHOULD wire one
// (NewEnforcerChecker) so an addon only receives events its manifest declared
// an `event:subscribe` capability for.
type CapabilityChecker interface {
	CanDeliver(addonKey, event string) error
}

// CapabilityCheckerFunc adapts a plain function to CapabilityChecker.
type CapabilityCheckerFunc func(addonKey, event string) error

// CanDeliver satisfies CapabilityChecker.
func (f CapabilityCheckerFunc) CanDeliver(addonKey, event string) error { return f(addonKey, event) }

// ErrNoCompiledRegistry is logged (not returned to the bus) when a "compiled"
// subscription fires but no CompiledRegistry was wired.
var ErrNoCompiledRegistry = errors.New("dispatch: compiled subscription but no CompiledRegistry wired")

// Wire is the OPT-IN entry point. It migrates the event_deliveries table,
// subscribes a trusted "kernel" wildcard handler to the bus, and returns a
// cancel func that unsubscribes (idempotent). Importing this package without
// calling Wire has zero effect — same contract as audit.Wire.
//
//	cancel, err := dispatch.Wire(bus, host.AsWasmInvoker(), db, provider,
//	    dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enforcer)),
//	    dispatch.WithCompiledRegistry(myRegistry),
//	)
//
// invoker may be nil only if every subscription the provider yields is
// "compiled"; a nil invoker with a "wasm" subscription logs and dead-letters
// that delivery. db is required (the delivery ledger lives there).
func Wire(bus *events.Bus, invoker WasmInvoker, db *gorm.DB, provider SubscriptionProvider, opts ...Option) (cancel func(), err error) {
	noop := func() {}
	if bus == nil {
		return noop, fmt.Errorf("dispatch: nil events.Bus")
	}
	if db == nil {
		return noop, fmt.Errorf("dispatch: nil *gorm.DB")
	}
	if provider == nil {
		return noop, fmt.Errorf("dispatch: nil SubscriptionProvider")
	}

	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	if err := Migrate(db, o.tableName); err != nil {
		return noop, fmt.Errorf("dispatch: migrate %s: %w", o.tableName, err)
	}

	d := &Dispatcher{
		bus:      bus,
		invoker:  invoker,
		db:       db,
		provider: provider,
		opts:     o,
		logger:   o.logger,
	}

	if err := bus.Subscribe(subscriberKey, "*", d.handle); err != nil {
		return noop, fmt.Errorf("dispatch: subscribe: %w", err)
	}
	d.logger.Info("dispatch.wired",
		slog.String("table", o.tableName),
		slog.Int("max_attempts", o.maxAttempts))

	cancel = func() { bus.Unsubscribe(subscriberKey) }
	return cancel, nil
}
