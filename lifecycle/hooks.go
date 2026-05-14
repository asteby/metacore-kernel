// hooks.go implements the runtime that reads `manifest.LifecycleHooks` and
// dispatches each declared `HookDef` against a registered HookDispatcher.
//
// The contract is the dual of dynamic.ActionDispatcher: each HookTarget.Type
// ("wasm" | "webhook") resolves to a dispatcher the host wires at boot; the
// kernel itself stays neutral and does not import the wasm runtime or
// net/http here. This keeps lifecycle/ a leaf package (only depends on
// manifest) and matches the layering installer / bridge already use.
//
// Two families of hook events are recognised:
//
//   - Lifecycle: "install" | "uninstall" | "enable" | "disable" | "upgrade".
//     Fired exactly once by the installer at the matching state transition.
//     Errors from a lifecycle hook abort the transition (the installer's
//     existing Addon.OnInstall semantics already follow this rule).
//
//   - CRUD: "before_create" | "after_create" | "before_update" |
//     "after_update" | "before_delete" | "after_delete". Fired by
//     dynamic.Service around row mutations through the dynamic.HookRegistry
//     adapter the installer registers at install time. `before_*` errors
//     abort the operation; `after_*` errors are logged and swallowed so a
//     misbehaving after-hook can't strand a committed row.
//
// HookDef.Async lets `after_*` hooks (and only those) fire-and-forget on a
// goroutine — convenient for slow notifications. `before_*` ignores Async by
// design because the operation has to wait for its veto.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// HookEvent enumerates the supported `manifest.LifecycleHooks` map keys. The
// values double as the on-the-wire identifiers — addon authors declare them
// verbatim in their manifest.
const (
	// Lifecycle transitions — fired by installer.Installer.
	HookEventInstall   = "install"
	HookEventUninstall = "uninstall"
	HookEventEnable    = "enable"
	HookEventDisable   = "disable"
	HookEventUpgrade   = "upgrade"

	// CRUD events — fired by dynamic.Service via the registered adapter.
	HookEventBeforeCreate = "before_create"
	HookEventAfterCreate  = "after_create"
	HookEventBeforeUpdate = "before_update"
	HookEventAfterUpdate  = "after_update"
	HookEventBeforeDelete = "before_delete"
	HookEventAfterDelete  = "after_delete"
)

// HookTargetType is the closed set of `HookTarget.Type` values the kernel
// knows how to route. "prompt" is reserved for a future LLM dispatcher and
// is accepted by validation but currently runs as a no-op when no
// dispatcher is registered (callers can wire a custom one through
// HookRunner.Dispatchers).
const (
	HookTargetWasm    = "wasm"
	HookTargetWebhook = "webhook"
	HookTargetPrompt  = "prompt"
)

// LifecycleHookEvents is the closed set of `install`-family events. The
// installer iterates this list at validation time and at dispatch time to
// catch typos before any DB work happens.
var LifecycleHookEvents = map[string]struct{}{
	HookEventInstall:   {},
	HookEventUninstall: {},
	HookEventEnable:    {},
	HookEventDisable:   {},
	HookEventUpgrade:   {},
}

// CRUDHookEvents is the closed set of `before_*`/`after_*` events. Same role
// as LifecycleHookEvents but for the dynamic CRUD plane.
var CRUDHookEvents = map[string]struct{}{
	HookEventBeforeCreate: {},
	HookEventAfterCreate:  {},
	HookEventBeforeUpdate: {},
	HookEventAfterUpdate:  {},
	HookEventBeforeDelete: {},
	HookEventAfterDelete:  {},
}

// IsBeforeEvent reports whether the event semantically vetoes the operation
// on error. Used by HookRunner.Run to decide between propagating the error
// (`before_*`, lifecycle transitions) or logging-and-swallowing (`after_*`).
func IsBeforeEvent(event string) bool {
	switch event {
	case HookEventBeforeCreate, HookEventBeforeUpdate, HookEventBeforeDelete:
		return true
	}
	// Lifecycle transitions are treated as "before" — the installer aborts
	// the transition on error, same as the existing Addon.OnInstall contract.
	if _, ok := LifecycleHookEvents[event]; ok {
		return true
	}
	return false
}

// HookRequest is the input to a HookDispatcher.
//
// AddonKey scopes the dispatch (per-installation wasm module, per-addon
// signed webhook secret). OrgID identifies the tenant — required for
// schema-per-tenant addons. Event is the matched hook key from the
// manifest. Target is the declared HookDef.Target, never nil. Payload is
// the JSON-encoded event-specific body — the installer encodes lifecycle
// transitions, the dynamic adapter encodes CRUD rows.
type HookRequest struct {
	AddonKey string
	OrgID    uuid.UUID
	Event    string
	Target   manifest.HookTarget
	Payload  []byte
}

// HookResponse is the dispatcher's reply. Only Error and Body are read by
// HookRunner today; future revisions may surface structured metadata for
// observability (latency, retry hint, etc.) without touching the
// dispatcher contract.
type HookResponse struct {
	Body []byte
}

// HookDispatcher routes a HookRequest to the addon's backend. One
// implementation per HookTarget.Type is registered via HookRunner.Register.
// Kernel-bundled hosts wire "wasm" (kernel/runtime/wasm) and "webhook"
// (kernel/security.WebhookDispatcher); custom hosts can supply more.
type HookDispatcher interface {
	Dispatch(ctx context.Context, req HookRequest) (HookResponse, error)
}

// HookDispatcherFunc adapts a plain function to HookDispatcher — useful for
// tests and ad-hoc host glue.
type HookDispatcherFunc func(ctx context.Context, req HookRequest) (HookResponse, error)

// Dispatch satisfies HookDispatcher.
func (f HookDispatcherFunc) Dispatch(ctx context.Context, req HookRequest) (HookResponse, error) {
	return f(ctx, req)
}

// ErrNoDispatcher is returned when a HookDef declares a Target.Type the
// runner has no dispatcher for. It is surfaced as an error from `before_*`
// (aborts the operation) and as a logged warning from `after_*`. Operators
// see it on every dispatch so a forgotten host wire-in is loud.
var ErrNoDispatcher = errors.New("lifecycle: no dispatcher registered for hook target type")

// HookRunner reads manifest.LifecycleHooks and dispatches matching hooks
// through the registered HookDispatchers. Safe for concurrent use after
// construction — Register is intended to be called once at boot.
type HookRunner struct {
	dispatchers map[string]HookDispatcher
	logger      *slog.Logger
}

// NewHookRunner constructs a runner with an empty dispatcher table. Hosts
// call Register("wasm", …) / Register("webhook", …) before Boot.
func NewHookRunner(logger *slog.Logger) *HookRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &HookRunner{
		dispatchers: make(map[string]HookDispatcher),
		logger:      logger,
	}
}

// Register binds a HookDispatcher to a HookTarget.Type. Calling Register
// twice for the same type replaces the previous binding — convenient for
// tests that wire a one-off dispatcher and reset between cases.
func (r *HookRunner) Register(targetType string, d HookDispatcher) {
	if r == nil || d == nil {
		return
	}
	r.dispatchers[targetType] = d
}

// HasDispatcher reports whether a dispatcher is wired for the given target
// type. Hosts and tests use it to decide whether to skip a hook silently or
// to surface a wiring gap.
func (r *HookRunner) HasDispatcher(targetType string) bool {
	if r == nil {
		return false
	}
	_, ok := r.dispatchers[targetType]
	return ok
}

// Run looks up `m.LifecycleHooks[event]`, sorts the hooks by Priority
// (ascending — lower numbers fire first), and dispatches each one in order.
//
// Error semantics:
//
//   - `before_*` and lifecycle events: the first error aborts the chain and
//     is returned to the caller. The caller (installer.Install,
//     dynamic.Service.Create, …) propagates it as an operation failure.
//
//   - `after_*` events: errors are logged through r.logger and swallowed.
//     The operation has already committed; refusing to log-and-continue
//     would strand a successful row behind a flaky notification hook.
//
// HookDef.Async detaches `after_*` dispatch onto a goroutine — useful when
// a notification target is slow but the addon owner does not want the
// commit path waiting on it. Async on a `before_*` / lifecycle event is a
// no-op (Validate rejects the combination so addon authors catch the
// contradiction at install time).
//
// A nil HookRunner is a no-op so hosts that have not wired one keep working
// — matches the pre-implementation behaviour where LifecycleHooks was
// purely declarative.
func (r *HookRunner) Run(ctx context.Context, addonKey string, orgID uuid.UUID, event string, m manifest.Manifest, payload []byte) error {
	if r == nil {
		return nil
	}
	hooks := matchingHooks(m, event)
	if len(hooks) == 0 {
		return nil
	}
	isBefore := IsBeforeEvent(event)
	for i := range hooks {
		h := hooks[i]
		req := HookRequest{
			AddonKey: addonKey,
			OrgID:    orgID,
			Event:    event,
			Target:   h.Target,
			Payload:  payload,
		}
		// Async only kicks in for `after_*` — `before_*` and lifecycle
		// transitions must block so the kernel can honour the veto.
		if h.Async && !isBefore {
			go r.dispatchOne(ctx, addonKey, event, req, false)
			continue
		}
		if err := r.dispatchOne(ctx, addonKey, event, req, isBefore); err != nil && isBefore {
			return err
		}
	}
	return nil
}

// matchingHooks returns the HookDefs declared for `event` keyed by Priority
// ascending. The slice is a copy so callers can mutate it without
// disturbing the manifest. Empty result is returned untouched — both nil
// LifecycleHooks and a missing key behave the same.
func matchingHooks(m manifest.Manifest, event string) []manifest.HookDef {
	defs := m.LifecycleHooks[event]
	if len(defs) == 0 {
		return nil
	}
	out := make([]manifest.HookDef, len(defs))
	copy(out, defs)
	// Insertion sort — hook chains are typically <5 entries, so the
	// O(n^2) constant is cheaper than allocating a sort.Slice closure.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Priority < out[j-1].Priority; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// dispatchOne resolves the dispatcher, invokes it, and either returns the
// error (when the caller wants to honour it — `before_*` and lifecycle) or
// logs it (when the caller is in `after_*` mode and a failure must not
// strand the operation).
func (r *HookRunner) dispatchOne(ctx context.Context, addonKey, event string, req HookRequest, propagate bool) error {
	d, ok := r.dispatchers[req.Target.Type]
	if !ok {
		err := fmt.Errorf("%w: %q", ErrNoDispatcher, req.Target.Type)
		if propagate {
			return err
		}
		r.logger.Warn("lifecycle.hook_dispatcher_missing",
			slog.String("addon_key", addonKey),
			slog.String("event", event),
			slog.String("target_type", req.Target.Type),
		)
		return nil
	}
	_, err := d.Dispatch(ctx, req)
	if err == nil {
		return nil
	}
	if propagate {
		return fmt.Errorf("lifecycle hook %s for %s: %w", event, addonKey, err)
	}
	r.logger.Warn("lifecycle.hook_failed",
		slog.String("addon_key", addonKey),
		slog.String("event", event),
		slog.String("target_type", req.Target.Type),
		slog.String("err", err.Error()),
	)
	return nil
}
