// backend_runtime.go declares the seam through which the installer asks a
// host to materialise an addon's backend runtime (today: WASM) right after
// the database state has been persisted.
//
// Layering — Law 3 — the kernel installer must not import runtime/wasm or
// host directly: runtime/wasm has heavier deps (wazero, pg_query for the
// db_query host import) and host already imports installer indirectly via
// bridge wiring. We declare a minimal interface here and provide a no-op
// default so:
//
//   - the kernel installer can call LoadBackendRuntime unconditionally;
//   - hosts wire a real implementation backed by host.LoadWASMFromBundle
//     (the existing helper that knows how to read backend/backend.wasm from
//     the bundle and register it on Host.WASM);
//   - tests inject a recording fake to assert the call without dragging the
//     full wasm runtime into installer tests.
//
// Cross-checking declared exports — when manifest.Backend.Exports is
// non-empty, the kernel feeds that list to the loader so it can refuse
// modules whose exported function set doesn't satisfy the declaration. The
// loader is the right place for the check because it already has the
// compiled module in hand; the installer just hands the list across.
package installer

import (
	"context"

	"github.com/asteby/metacore-kernel/bundle"
)

// BackendRuntimeLoader is the interface the installer calls after a
// successful Install / Upgrade when the bundle declares an in-process
// backend runtime (manifest.Backend.Runtime != "" && != "webhook"). One
// implementation per host runtime — today only WASM ships, but the
// interface leaves room for the reserved "binary" runtime.
//
// LoadFromBundle MUST:
//
//   - Be idempotent — Install/Upgrade can re-fire it for the same addon key.
//   - Refuse mismatched runtimes (Backend.Runtime not the impl's responsibility)
//     by returning an error that the installer wraps with the addon key.
//   - Cross-check declared exports when Backend.Exports is non-empty (the
//     same shape the v0.18.0 wasm action trigger validator uses).
//
// LoadFromBundle is called BEFORE the manifest-change broadcast so the
// frontend's hot-reload path is the last thing to fire; that way the
// browser never sees a "new version available" signal pointing at a
// half-loaded backend.
type BackendRuntimeLoader interface {
	LoadFromBundle(ctx context.Context, b *bundle.Bundle) error
}

// NoopBackendRuntimeLoader is the zero-cost default. Hosts that don't run
// in-process WASM (CLI tools, schema-only smoke tests) keep this so the
// installer's fire-site is unconditional and exception-free.
type NoopBackendRuntimeLoader struct{}

// LoadFromBundle satisfies BackendRuntimeLoader by doing nothing.
func (NoopBackendRuntimeLoader) LoadFromBundle(context.Context, *bundle.Bundle) error {
	return nil
}

// BackendRuntimeVerifier is the optional second half of a loader: after
// LoadFromBundle it lets the installer ask whether the loaded module really
// provides what the manifest declared (backend.exports). A host whose loader
// is the wasm adapter gets it for free.
//
// Why it exists: Load keeps a compiled module when the bytes are unchanged and
// only swaps the manifest spec, so a manifest that gained exports (new API
// routes, new triggers) can end up in front of a module that lacks them. The
// symptom is a 501 "route handler is not available" on every new route until
// the addon is reinstalled. The installer now detects it right after the load.
type BackendRuntimeVerifier interface {
	// VerifyLoaded returns the declared exports the loaded module lacks
	// (empty = coherent).
	VerifyLoaded(ctx context.Context, b *bundle.Bundle) (missing []string, err error)
}

// BackendRuntimeReloader forces a fresh compile of the bundle's module,
// discarding the cached one. Used once per load when verification fails.
type BackendRuntimeReloader interface {
	ReloadFromBundle(ctx context.Context, b *bundle.Bundle) error
}

// BackendDriftNotifier receives the outcome of a drift check ("reloaded" or
// "persisting"), for metrics.
type BackendDriftNotifier interface {
	NoteDrift(addonKey, outcome string)
}
