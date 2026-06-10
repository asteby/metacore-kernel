package host

import (
	"context"
	"fmt"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/runtime/wasm"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// EnableWASM attaches a WASM runtime to the host so that installed addons
// declaring `manifest.backend.runtime = "wasm"` can be loaded and invoked
// in-process. Safe to call once at boot; idempotent — second call returns
// the existing runtime.
//
// caps is the policy the runtime applies to host imports (http_fetch,
// env_get, log). Each invocation is additionally bounded by the spec's
// timeout + memory limits declared in the manifest.
func (h *Host) EnableWASM(ctx context.Context, caps *security.Capabilities) error {
	if h.WASM != nil {
		return nil
	}
	wh, err := wasm.NewHost(ctx, caps, nil)
	if err != nil {
		return fmt.Errorf("host: enable wasm: %w", err)
	}
	h.WASM = wh
	// Hand the installer the loader so manifest-declared in-process
	// backends (runtime=wasm) get compiled + registered automatically at
	// install / upgrade time — no host-side boilerplate. Hosts that boot
	// the installer without EnableWASM keep the default no-op loader.
	if h.Installer != nil {
		h.Installer.WithBackendRuntime(&wasmBackendLoader{host: h})
	}
	return nil
}

// wasmBackendLoader is the host-side adapter that closes the
// installer.BackendRuntimeLoader interface over the existing
// host.LoadWASMFromBundle helper. Defined here (not in the installer)
// because runtime/wasm pulls in heavier deps the kernel layer
// deliberately avoids.
type wasmBackendLoader struct {
	host *Host
}

// LoadFromBundle delegates to host.LoadWASMFromBundle so the existing
// path (extract backend.wasm → Host.WASM.Load with cross-checked
// exports) keeps working unchanged. The installer treats binary-runtime
// addons as "needs runtime"; the helper returns
// ErrRuntimeNotImplemented for that case so the installer surfaces the
// right error.
func (l *wasmBackendLoader) LoadFromBundle(ctx context.Context, b *bundle.Bundle) error {
	if l == nil || l.host == nil {
		return fmt.Errorf("wasmBackendLoader: host is nil")
	}
	return l.host.LoadWASMFromBundle(ctx, b)
}

// ErrRuntimeNotImplemented is returned by LoadWASMFromBundle when the bundle
// declares `manifest.backend.runtime = "binary"`. The value is reserved in
// the ABI v1.0 freeze for a future native side-car runtime; manifest.Validate
// accepts it (so addon authors can declare it ahead of time) but the kernel
// has no installer or invoker for it yet. The error wraps a sentinel so call
// sites can branch on `errors.Is(err, ErrRuntimeNotImplemented)` without
// string-matching.
//
// See manifest.ValidateAdvisory for an authoring-time warning that flags the
// same case before the bundle even reaches the install pipeline.
var ErrRuntimeNotImplemented = fmt.Errorf("runtime_not_implemented")

// LoadWASMFromBundle extracts backend/backend.wasm from a bundle and registers
// it under the addon's key. Call right after the rest of the install pipeline
// so migrations + frontend + capabilities are already in place.
//
// The function returns ErrRuntimeNotImplemented when the manifest declares
// `backend.runtime = "binary"` — the value is reserved in the ABI v1.0 freeze
// but there is no executor for it yet, so the installer must refuse a
// binary-runtime addon up front rather than silently install a dead bundle.
func (h *Host) LoadWASMFromBundle(ctx context.Context, b *bundle.Bundle) error {
	if h.WASM == nil {
		return fmt.Errorf("host: WASM runtime not enabled (call EnableWASM first)")
	}
	m := &b.Manifest
	if m.Backend != nil && m.Backend.Runtime == "binary" {
		return fmt.Errorf("host: addon %q declares backend.runtime=%q: %w",
			m.Key, m.Backend.Runtime, ErrRuntimeNotImplemented)
	}
	if m.Backend == nil || m.Backend.Runtime != "wasm" {
		return nil // nothing to do — webhook or native addon
	}
	entry := m.Backend.Entry
	if entry == "" {
		entry = "backend/backend.wasm"
	}
	wasmBytes, ok := b.Backend[entry]
	if !ok {
		return fmt.Errorf("host: bundle missing %q for addon %q", entry, m.Key)
	}
	return h.WASM.Load(ctx, m.Key, wasmBytes, m.Backend)
}

// InvokeWASM is a thin helper over Host.WASM.Invoke that binds the
// installation's stored settings automatically. Returns the raw byte slice
// the guest export wrote into its linear memory — caller unmarshals.
//
// Tenant-scoped callers SHOULD prefer InvokeWASMFor (or wrap ctx with
// runtime/wasm.WithOrgID before reaching here) so the guest's event_emit
// import resolves the right orgID; see docs/wasm-abi.md § 12.6.
func (h *Host) InvokeWASM(ctx context.Context, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	if h.WASM == nil {
		return nil, fmt.Errorf("host: WASM runtime not enabled")
	}
	return h.WASM.Invoke(ctx, installation, addonKey, funcName, payload, settings)
}

// InvokeWASMFor is the tenant-aware sibling of InvokeWASM. It threads orgID
// through the runtime/wasm context bag so the guest's event_emit import
// publishes against the caller's tenant. Pass uuid.Nil only when the
// invocation legitimately has no active org (kernel boot probes, fixtures);
// event_emit then surfaces `no_active_org` if the guest publishes.
func (h *Host) InvokeWASMFor(ctx context.Context, orgID uuid.UUID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	if h.WASM == nil {
		return nil, fmt.Errorf("host: WASM runtime not enabled")
	}
	return h.WASM.InvokeFor(ctx, orgID, installation, addonKey, funcName, payload, settings)
}

// WASMInvoker returns the host's wasm runtime as a dispatch.WasmInvoker so a
// host can wire the event-subscription dispatcher in one line:
//
//	cancel, err := dispatch.Wire(bus, h.WASMInvoker(), db, provider,
//	    dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enforcer)))
//
// Returns a real nil interface when the wasm runtime is not enabled (so the
// dispatcher's `invoker == nil` guard fires and a wasm subscription
// dead-letters cleanly rather than panicking on a typed-nil). Call EnableWASM
// first if any addon ships a wasm subscription handler. Kept here (not in the
// dispatch package) so dispatch stays free of the runtime/wasm CGO dependency.
func (h *Host) WASMInvoker() dispatch.WasmInvoker {
	if h.WASM == nil {
		return nil
	}
	return h.WASM
}

// manifestBackend is re-exported for call sites that only import kernel/host.
type BackendSpec = manifest.BackendSpec
