package wasm

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// The global policy is a single static one shared by every addon, so a
// permissive global grant (ops runs `connector:read *`) let ANY installed addon
// read ANY connector the org configured. WithAddonCaps stamps each invocation
// with the invoked addon's own policy instead.
func TestCapsFor_PerAddonPolicyOverridesGlobal(t *testing.T) {
	global := security.Compile("", []manifest.Capability{
		{Kind: "connector:read", Target: "*"},
	})
	perAddon := map[string]*security.Capabilities{
		// owns factura_com
		"fiscal_mexico": security.Compile("fiscal_mexico", []manifest.Capability{
			{Kind: "connector:read", Target: "factura_com"},
		}),
		// declares reuse of another addon's connector — the supported case
		"waybill_cartaporte": security.Compile("waybill_cartaporte", []manifest.Capability{
			{Kind: "connector:read", Target: "factura_com"},
		}),
		// declares nothing
		"evil_addon": security.Compile("evil_addon", nil),
	}
	h := &Host{caps: global}
	h.WithAddonCaps(func(key string) *security.Capabilities { return perAddon[key] })

	if err := h.capsFor("fiscal_mexico").CanReadConnector("factura_com"); err != nil {
		t.Fatalf("owner must read its own connector: %v", err)
	}
	if err := h.capsFor("waybill_cartaporte").CanReadConnector("factura_com"); err != nil {
		t.Fatalf("declared cross-addon reuse must resolve: %v", err)
	}
	if err := h.capsFor("evil_addon").CanReadConnector("factura_com"); err == nil {
		t.Fatal("an addon that declares no connector:read must NOT read another addon's connector (issue ops#870)")
	}
	// http:fetch was the same hole: ops unions every installed addon's hosts.
	if err := h.capsFor("evil_addon").CanFetch("https://api.factura.com/stamp"); err == nil {
		t.Fatal("an addon must not fetch a host it did not declare")
	}
}

// An unknown addon falls back to the global policy, so wiring a resolver can
// never reintroduce the nil *security.Capabilities that panicked connector_get.
func TestCapsFor_FallsBackToGlobal(t *testing.T) {
	global := security.Compile("", []manifest.Capability{
		{Kind: "connector:read", Target: "github"},
	})
	h := &Host{caps: global}
	h.WithAddonCaps(func(string) *security.Capabilities { return nil })

	got := h.capsFor("not_installed")
	if got == nil {
		t.Fatal("capsFor returned nil — connector_get would nil-deref")
	}
	if err := got.CanReadConnector("github"); err != nil {
		t.Fatalf("unknown addon should fall back to the global policy: %v", err)
	}
}

// No resolver wired at all keeps the pre-change behaviour exactly.
func TestCapsFor_NoResolverUsesGlobal(t *testing.T) {
	global := security.Compile("", nil)
	h := &Host{caps: global}
	if h.capsFor("anything") != global {
		t.Fatal("without a resolver every invocation must run under the global policy")
	}
}
