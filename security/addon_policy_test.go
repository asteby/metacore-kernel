package security_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// manifest.ConnectorAccessFor owns the rule and is tested there; this covers the
// wiring — that what the rule authorises actually reaches the compiled policy
// the wasm gate consults, which is the seam where secrets:read used to fall
// through (valid in the schema, no case in Compile, granted nothing).
func TestCompileForAddon_RuleReachesTheGate(t *testing.T) {
	// waybill-cartaporte, exactly as published: reuses another addon's PAC
	// connector via secrets:read.
	caps, access := security.CompileForAddon("waybill_cartaporte", manifest.Manifest{
		Capabilities: []manifest.Capability{
			{Kind: "http:fetch", Target: "api.factura.com"},
			{Kind: "secrets:read", Target: "factura_com"},
		},
	})
	if err := caps.CanReadConnector("factura_com"); err != nil {
		t.Fatalf("secrets:read must reach the gate: %v", err)
	}
	if err := caps.CanFetch("https://api.factura.com/timbrar"); err != nil {
		t.Fatalf("declared host must reach the gate: %v", err)
	}
	if err := caps.CanReadConnector("github"); err == nil {
		t.Fatal("declaring one connector must not grant the others")
	}
	if len(access.Implicit) != 0 {
		t.Fatalf("explicit declaration must not report implicit reliance: %v", access.Implicit)
	}
}

// The implicit owner grant must reach the gate too, or fiscal_mexico stops
// stamping every CFDI 4.0 the moment this ships.
func TestCompileForAddon_ImplicitOwnerGrantReachesTheGate(t *testing.T) {
	caps, access := security.CompileForAddon("fiscal_mexico", manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "http:fetch", Target: "api.factura.com"}},
		Connectors:   []manifest.ConnectorDef{{Key: "factura_com"}},
	})
	if err := caps.CanReadConnector("factura_com"); err != nil {
		t.Fatalf("owner must keep reading its own connector: %v", err)
	}
	if len(access.Implicit) != 1 || access.Implicit[0] != "factura_com" {
		t.Fatalf("the reliance must be reported so the host can log it: %v", access.Implicit)
	}
}

// The ops#870 regression, through the compiled policy.
func TestCompileForAddon_UndeclaredDenied(t *testing.T) {
	caps, _ := security.CompileForAddon("evil_addon", manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "db:read", Target: "addon_evil_addon.*"}},
	})
	if err := caps.CanReadConnector("factura_com"); err == nil {
		t.Fatal("an addon that declares nothing must not read another addon's connector")
	}
	if err := caps.CanFetch("https://api.factura.com/x"); err == nil {
		t.Fatal("an addon must not fetch a host it did not declare")
	}
}

// A refused wildcard must not compile into a grant.
func TestCompileForAddon_WildcardNotCompiled(t *testing.T) {
	caps, access := security.CompileForAddon("greedy", manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "secrets:read", Target: "*"}},
	})
	if err := caps.CanReadConnector("factura_com"); err == nil {
		t.Fatal("a wildcard must not compile into a connector grant")
	}
	if len(access.Refused) != 1 {
		t.Fatalf("the refusal must be surfaced: %v", access.Refused)
	}
}

// Non-connector capabilities must pass through untouched, including the
// self-schema grants Compile adds.
func TestCompileForAddon_PassesThroughOtherKinds(t *testing.T) {
	caps, _ := security.CompileForAddon("billing", manifest.Manifest{
		Capabilities: []manifest.Capability{
			{Kind: "db:read", Target: "orders"},
			{Kind: "event:emit", Target: "billing.*"},
		},
	})
	if err := caps.CanReadModel("orders"); err != nil {
		t.Fatalf("db:read must survive: %v", err)
	}
	if err := caps.CanEmit("billing.invoiced"); err != nil {
		t.Fatalf("event:emit must survive: %v", err)
	}
	if err := caps.CanWriteModel("addon_billing.invoices"); err != nil {
		t.Fatalf("self-schema grant must survive: %v", err)
	}
}
