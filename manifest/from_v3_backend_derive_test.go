package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// wasmAddonManifestJSON is a v3 manifest for an addon that declares a wasm
// subscription handler and a wasm action handler. The schema is
// additionalProperties:false at the top level, so there is NO top-level
// "backend" block — the BackendSpec must be derived from the handlers.
const wasmAddonManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "tire_warranty", "name": "Tire Warranty", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "capabilities": [
    { "kind": "event:subscribe", "target": "sale.created" },
    { "kind": "event:emit",      "target": "warranty.issued" }
  ],
  "contributions": {
    "subscriptions": [
      {
        "event": "sale.created",
        "handler": { "type": "wasm", "function": "on_sale_created" }
      }
    ],
    "actions": [
      {
        "key": "issue_warranty",
        "label": "Issue Warranty",
        "target_model": "Sale",
        "handler": { "type": "wasm", "function": "issue_warranty" }
      }
    ]
  }
}`

// TestFromV3_DerivesBackendFromWasmHandlers verifies that FromV3 synthesises a
// BackendSpec{Runtime:"wasm"} when contributions declare at least one handler
// with type "wasm".  The Exports slice must contain every distinct function
// name from those handlers (deduplicated).
func TestFromV3_DerivesBackendFromWasmHandlers(t *testing.T) {
	m, err := v3.Parse([]byte(wasmAddonManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)

	if out.Backend == nil {
		t.Fatal("expected Backend to be derived, got nil — LoadWASMFromBundle will skip the bundle")
	}
	if out.Backend.Runtime != "wasm" {
		t.Fatalf("Backend.Runtime = %q, want %q", out.Backend.Runtime, "wasm")
	}

	wantFunctions := map[string]bool{
		"on_sale_created": true,
		"issue_warranty":  true,
	}
	if len(out.Backend.Exports) != len(wantFunctions) {
		t.Fatalf("Backend.Exports = %v (len %d), want exactly %v",
			out.Backend.Exports, len(out.Backend.Exports), wantFunctions)
	}
	for _, fn := range out.Backend.Exports {
		if !wantFunctions[fn] {
			t.Errorf("Backend.Exports contains unexpected function %q", fn)
		}
	}
}

// TestFromV3_DerivesBackendSubscriptionOnly verifies the derivation when only
// a subscription handler is declared (no action handler).
func TestFromV3_DerivesBackendSubscriptionOnly(t *testing.T) {
	const subscriptionOnly = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "sub_only", "name": "Sub Only", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "subscriptions": [
      {
        "event": "order.placed",
        "handler": { "type": "wasm", "function": "handle_order" }
      }
    ]
  }
}`
	m, err := v3.Parse([]byte(subscriptionOnly))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)

	if out.Backend == nil {
		t.Fatal("expected Backend to be derived for subscription-only wasm addon, got nil")
	}
	if out.Backend.Runtime != "wasm" {
		t.Fatalf("Backend.Runtime = %q, want %q", out.Backend.Runtime, "wasm")
	}
	if len(out.Backend.Exports) != 1 || out.Backend.Exports[0] != "handle_order" {
		t.Fatalf("Backend.Exports = %v, want [handle_order]", out.Backend.Exports)
	}
}

// TestFromV3_DerivesBackendDeduplicates ensures that a function named in both
// an action handler and a subscription handler appears only once in Exports.
func TestFromV3_DerivesBackendDeduplicates(t *testing.T) {
	const dupManifest = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "dup_test", "name": "Dup Test", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "subscriptions": [
      { "event": "x.created", "handler": { "type": "wasm", "function": "process" } }
    ],
    "actions": [
      {
        "key": "do_it",
        "target_model": "X",
        "handler": { "type": "wasm", "function": "process" }
      }
    ]
  }
}`
	m, err := v3.Parse([]byte(dupManifest))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)

	if out.Backend == nil {
		t.Fatal("expected Backend, got nil")
	}
	if len(out.Backend.Exports) != 1 {
		t.Fatalf("Backend.Exports = %v, want exactly 1 deduplicated entry", out.Backend.Exports)
	}
	if out.Backend.Exports[0] != "process" {
		t.Fatalf("Backend.Exports[0] = %q, want %q", out.Backend.Exports[0], "process")
	}
}

// TestFromV3_NoBackendForWebhookOnlyAddon confirms that a manifest whose
// handlers are all type:"webhook" does NOT get a wasm BackendSpec.
func TestFromV3_NoBackendForWebhookOnlyAddon(t *testing.T) {
	const webhookManifest = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "webhook_addon", "name": "Webhook Addon", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "subscriptions": [
      {
        "event": "order.placed",
        "handler": { "type": "webhook", "url": "https://example.com/hook" }
      }
    ]
  }
}`
	m, err := v3.Parse([]byte(webhookManifest))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	if out.Backend != nil {
		t.Fatalf("expected Backend=nil for webhook-only addon, got %+v", out.Backend)
	}
}

// TestFromV3_NoBackendForNoContributions confirms that a manifest with no
// contributions at all does not get a BackendSpec.
func TestFromV3_NoBackendForNoContributions(t *testing.T) {
	const plainManifest = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "plain", "name": "Plain", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] }
}`
	m, err := v3.Parse([]byte(plainManifest))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	if out.Backend != nil {
		t.Fatalf("expected Backend=nil for addon with no contributions, got %+v", out.Backend)
	}
}
