package installer

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/manifest"
)

// compiledHandlerManifest declares a wasm action, a compiled action, a compiled
// tool and a compiled subscription so the extractor must pick exactly the three
// compiled refs (and skip the wasm one).
const compiledHandlerManifest = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "contributions": {
    "actions": [
      { "key": "wasm_act", "handler": { "type": "wasm", "function": "WasmAct" } },
      { "key": "stamp", "handler": { "type": "compiled", "function": "StampInvoice" } }
    ],
    "tools": [
      { "key": "lookup", "description": "x", "handler": { "type": "compiled", "function": "LookupCustomer" } }
    ],
    "subscriptions": [
      { "event": "sale.created", "handler": { "type": "compiled", "function": "OnSale" } }
    ]
  }
}`

func bundleWithRaw(t *testing.T, raw string) *bundle.Bundle {
	t.Helper()
	return &bundle.Bundle{
		Manifest:    manifest.Manifest{Key: "billing", Version: "0.1.0"},
		RawManifest: []byte(raw),
	}
}

func TestExtractCompiledHandlers(t *testing.T) {
	refs := extractCompiledHandlers(bundleWithRaw(t, compiledHandlerManifest))
	got := map[string]bool{}
	for _, r := range refs {
		got[r.Function] = true
	}
	if len(refs) != 3 {
		t.Fatalf("extracted %d compiled refs, want 3: %+v", len(refs), refs)
	}
	for _, want := range []string{"StampInvoice", "LookupCustomer", "OnSale"} {
		if !got[want] {
			t.Errorf("missing compiled handler %q in %+v", want, got)
		}
	}
	if got["WasmAct"] {
		t.Error("wasm handler leaked into compiled refs")
	}
}

func TestValidateCompiledHandlersNilRegistryIsSoft(t *testing.T) {
	// No registry → no error (warning path), even with unresolved handlers.
	if err := validateCompiledHandlers(bundleWithRaw(t, compiledHandlerManifest), nil); err != nil {
		t.Fatalf("nil registry should warn-not-fail, got error: %v", err)
	}
}

func TestValidateCompiledHandlersHardFailsOnMissing(t *testing.T) {
	// Registry knows only StampInvoice → the other two must be reported.
	reg := CompiledHandlerRegistryFunc(func(fn string) bool { return fn == "StampInvoice" })
	err := validateCompiledHandlers(bundleWithRaw(t, compiledHandlerManifest), reg)
	if err == nil {
		t.Fatal("expected a hard error for unresolved compiled handlers, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"LookupCustomer", "OnSale"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should name missing handler %q; got: %s", want, msg)
		}
	}
	if strings.Contains(msg, "StampInvoice") {
		t.Errorf("error should NOT name the resolved handler StampInvoice; got: %s", msg)
	}
}

func TestValidateCompiledHandlersPassesWhenAllRegistered(t *testing.T) {
	reg := CompiledHandlerRegistryFunc(func(string) bool { return true })
	if err := validateCompiledHandlers(bundleWithRaw(t, compiledHandlerManifest), reg); err != nil {
		t.Fatalf("all handlers registered should pass, got: %v", err)
	}
}

func TestValidateCompiledHandlersFlagsEmptyFunction(t *testing.T) {
	const raw = `{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
      "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
      "contributions": {
        "actions": [ { "key": "broken", "handler": { "type": "compiled" } } ]
      }
    }`
	reg := CompiledHandlerRegistryFunc(func(string) bool { return true })
	err := validateCompiledHandlers(bundleWithRaw(t, raw), reg)
	if err == nil || !strings.Contains(err.Error(), "no function symbol") {
		t.Fatalf("expected an empty-function error, got: %v", err)
	}
}

func TestValidateCompiledHandlersNoCompiledIsNoop(t *testing.T) {
	const raw = `{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
      "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
      "contributions": {
        "actions": [ { "key": "w", "handler": { "type": "wasm", "function": "W" } } ]
      }
    }`
	// Even with a registry that rejects everything, a manifest with NO compiled
	// handlers must pass (zero-cost gate for wasm/webhook addons).
	reg := CompiledHandlerRegistryFunc(func(string) bool { return false })
	if err := validateCompiledHandlers(bundleWithRaw(t, raw), reg); err != nil {
		t.Fatalf("manifest without compiled handlers should be a no-op, got: %v", err)
	}
}
