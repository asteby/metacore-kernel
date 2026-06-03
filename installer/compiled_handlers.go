package installer

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/asteby/metacore-kernel/bundle"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// CompiledHandlerRegistry reports whether a compiled handler function symbol is
// known to the host. It is the bootstrap-validation seam for S7: addons can
// declare `handler: { type: "compiled", function: "<symbol>" }` on actions,
// tools and subscriptions, but the actual Go implementation lives in the HOST
// (ops registers its compiled functions in a map). The kernel cannot resolve
// the symbol itself, so a host that wants install-time validation wires its
// registry here and the installer fails the install with a clear message when
// a declared compiled handler has no implementation — instead of the addon
// installing "successfully" and every dispatch 403-ing at runtime because the
// function is missing.
//
// Has reports whether `function` is registered. The implementation is expected
// to be cheap (a map lookup). A nil registry disables the gate (the installer
// downgrades to a logged warning — see validateCompiledHandlers).
type CompiledHandlerRegistry interface {
	Has(function string) bool
}

// CompiledHandlerRegistryFunc adapts a plain func to CompiledHandlerRegistry so
// a host can wire a closure over its handler map without declaring a type:
//
//	inst.CompiledHandlers = installer.CompiledHandlerRegistryFunc(
//	    func(fn string) bool { _, ok := myHandlers[fn]; return ok })
type CompiledHandlerRegistryFunc func(function string) bool

// Has implements CompiledHandlerRegistry.
func (f CompiledHandlerRegistryFunc) Has(function string) bool { return f(function) }

// compiledHandlerType is the v3 handler.type value whose function symbol the
// host resolves against its compiled-in Go registry (as opposed to "wasm",
// dispatched to the addon's module, or "webhook", dispatched over HTTP).
const compiledHandlerType = "compiled"

// compiledHandlerRef is one declared compiled handler plus where it came from,
// so a validation error can point the author at the exact contribution.
type compiledHandlerRef struct {
	Function string // the declared handler.function symbol
	Where    string // human-readable origin, e.g. `contributions.actions[2] "stamp"`
}

// extractCompiledHandlers walks a bundle's ORIGINAL v3 manifest (RawManifest)
// and returns every action / tool / subscription handler whose type is
// "compiled", with its declared function symbol. It reads the raw bytes rather
// than the legacy manifest.Manifest because FromV3 collapses the handler type
// into a Trigger and drops the "compiled" discriminator entirely — the v3
// document is the only place the compiled-vs-wasm-vs-webhook distinction
// survives. A bundle with no RawManifest (in-memory / legacy v2) yields no refs
// and is treated as "nothing to validate".
//
// A handler typed "compiled" with an EMPTY function is itself a defect (there
// is no symbol to resolve) and is reported with Function="" so the caller can
// surface "declares a compiled handler with no function".
func extractCompiledHandlers(b *bundle.Bundle) []compiledHandlerRef {
	if b == nil || len(b.RawManifest) == 0 {
		return nil
	}
	m, err := v3.Parse(b.RawManifest)
	if err != nil || m == nil || m.Contributions == nil {
		// A RawManifest that no longer parses as v3 (or carries no
		// contributions) has nothing to validate here; the install flow's own
		// ValidateAdvisory already gates structural problems.
		return nil
	}
	var out []compiledHandlerRef
	add := func(h v3.Handler, where string) {
		if h.Type == compiledHandlerType {
			out = append(out, compiledHandlerRef{Function: h.Function, Where: where})
		}
	}
	for i, a := range m.Contributions.Actions {
		add(a.Handler, fmt.Sprintf("contributions.actions[%d] %q", i, a.Key))
	}
	for i, t := range m.Contributions.Tools {
		add(t.Handler, fmt.Sprintf("contributions.tools[%d] %q", i, t.Key))
	}
	for i, s := range m.Contributions.Subscriptions {
		add(s.Handler, fmt.Sprintf("contributions.subscriptions[%d] event=%q", i, s.Event))
	}
	return out
}

// validateCompiledHandlers is the S7 bootstrap gate. It collects the compiled
// handlers an addon declares and, depending on whether the host wired a
// registry, either:
//
//   - registry != nil — HARD-VALIDATES: returns an error naming every declared
//     compiled handler whose function symbol is missing (or empty) so the
//     install fails fast with operator-actionable guidance instead of leaking
//     into a runtime 403 on first dispatch.
//   - registry == nil — SOFT-WARNS: logs one warning per declared compiled
//     handler listing the symbols the host MUST provide, then proceeds. This is
//     the conservative default so hosts that have not yet adopted the registry
//     (or genuinely register handlers lazily) are not blocked — the warning
//     still surfaces the contract in the install audit trail.
//
// Returns nil when the addon declares no compiled handlers (the common case for
// wasm/webhook/declarative addons), so the gate is zero-cost for them.
func validateCompiledHandlers(b *bundle.Bundle, registry CompiledHandlerRegistry) error {
	refs := extractCompiledHandlers(b)
	if len(refs) == 0 {
		return nil
	}
	addonKey := ""
	if b != nil {
		addonKey = b.Manifest.Key
	}

	if registry == nil {
		// Soft path: no registry to check against. Warn (deduped) so the
		// missing-implementation risk is visible without blocking the install.
		for _, fn := range dedupeSortedFunctions(refs) {
			slog.Warn("installer.compiled_handler_unverified",
				"addon", addonKey,
				"function", fn,
				"hint", "host did not wire a CompiledHandlerRegistry; this compiled handler is NOT verified to exist and will 403 at dispatch if the host has no implementation")
		}
		return nil
	}

	// Hard path: every declared compiled handler must resolve.
	var missing []string
	for _, r := range refs {
		if r.Function == "" {
			missing = append(missing, fmt.Sprintf("%s declares a compiled handler with no function symbol", r.Where))
			continue
		}
		if !registry.Has(r.Function) {
			missing = append(missing, fmt.Sprintf("%s references compiled handler %q which is not registered with the host", r.Where, r.Function))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"installer: addon %q declares compiled handler(s) the host cannot resolve — register them before install or the action will 403 at dispatch:\n  - %s",
		addonKey, joinLines(missing))
}

// dedupeSortedFunctions returns the distinct, sorted function symbols across the
// refs (empty symbols rendered as the literal "<empty>") for stable warning
// output.
func dedupeSortedFunctions(refs []compiledHandlerRef) []string {
	seen := map[string]struct{}{}
	for _, r := range refs {
		fn := r.Function
		if fn == "" {
			fn = "<empty>"
		}
		seen[fn] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for fn := range seen {
		out = append(out, fn)
	}
	sort.Strings(out)
	return out
}

// joinLines joins validation messages with the same "\n  - " separator the rest
// of the install errors use, kept local so this file has no cross-package dep
// just for a string join.
func joinLines(msgs []string) string {
	out := ""
	for i, m := range msgs {
		if i > 0 {
			out += "\n  - "
		}
		out += m
	}
	return out
}
