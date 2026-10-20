package wasm

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// MissingExports reports which functions the manifest declares in
// backend.exports that the compiled module does NOT export. loaded is false
// when the addon has no compiled module.
//
// A non-empty result is the "new manifest, old module" state: routes and
// triggers resolve to an export the guest cannot run, and callers see 501 /
// "function not found" until the module is replaced. Load logs it, and the
// installer (installer.BackendRuntimeVerifier) reloads once and warns.
func (h *Host) MissingExports(addonKey string) (missing []string, loaded bool) {
	ce, ok := h.compiled.Load(addonKey)
	if !ok {
		return nil, false
	}
	entry, _ := ce.(*compiledEntry)
	if entry == nil {
		return nil, false
	}
	return missingExports(entry), true
}

func missingExports(entry *compiledEntry) []string {
	spec := entry.spec.Load()
	if spec == nil || len(spec.Exports) == 0 {
		return nil
	}
	have := entry.mod.ExportedFunctions()
	var out []string
	for _, name := range spec.Exports {
		if _, ok := have[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ExportDrift returns, per loaded addon, the declared exports its compiled
// module lacks. Addons without drift are omitted. Diagnostic surface for the
// host's cache-status endpoint.
func (h *Host) ExportDrift() map[string][]string {
	out := map[string][]string{}
	h.compiled.Range(func(k, v any) bool {
		if entry, _ := v.(*compiledEntry); entry != nil {
			if m := missingExports(entry); len(m) > 0 {
				out[k.(string)] = m
			}
		}
		return true
	})
	return out
}

// Reload replaces the addon's compiled module unconditionally: it drops the
// compiled entry and every instance pool, then compiles wasmBytes afresh. Load
// skips the work when the bytes are unchanged; Reload exists for the case where
// the cached state itself is suspect (see MissingExports).
func (h *Host) Reload(ctx context.Context, addonKey string, wasmBytes []byte, spec *manifest.BackendSpec) error {
	if err := h.Unload(ctx, addonKey, uuid.Nil); err != nil {
		return fmt.Errorf("wasm: reload %s: unload: %w", addonKey, err)
	}
	return h.Load(ctx, addonKey, wasmBytes, spec)
}

// warnDrift logs the manifest/module mismatch found right after a Load.
func (h *Host) warnDrift(addonKey string, missing []string) {
	slog.Warn("wasm.export_drift",
		"addon", addonKey, "missing_exports", missing,
		"hint", "manifest declares exports the loaded module lacks: routes/triggers bound to them answer 501 until the module is replaced")
}

// NoteDrift counts a drift outcome ("reloaded" when a reload cured it,
// "persisting" when the bundle itself lacks the declared exports).
func (h *Host) NoteDrift(addonKey, outcome string) { h.metrics.drift(addonKey, outcome) }
