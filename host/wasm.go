package host

import (
	"context"
	"fmt"

	"github.com/asteby/metacore-sdk/pkg/bundle"
	"github.com/asteby/metacore-sdk/pkg/manifest"
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
	return nil
}

// LoadWASMFromBundle extracts backend/backend.wasm from a bundle and registers
// it under the addon's key. Call right after the rest of the install pipeline
// so migrations + frontend + capabilities are already in place.
func (h *Host) LoadWASMFromBundle(ctx context.Context, b *bundle.Bundle) error {
	if h.WASM == nil {
		return fmt.Errorf("host: WASM runtime not enabled (call EnableWASM first)")
	}
	m := &b.Manifest
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
func (h *Host) InvokeWASM(ctx context.Context, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	if h.WASM == nil {
		return nil, fmt.Errorf("host: WASM runtime not enabled")
	}
	return h.WASM.Invoke(ctx, installation, addonKey, funcName, payload, settings)
}

// manifestBackend is re-exported for call sites that only import kernel/host.
type BackendSpec = manifest.BackendSpec
