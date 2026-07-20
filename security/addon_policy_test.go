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

// A connector declared in the manifest's own `connectors` block, with no
// `connector:read` capability, must NOT reach the gate. That block defines the
// connector and drives its configuration form in the Installed view; it is not
// an authorisation. link-inbox (channel_gateway) and link-agents (llm) are
// exactly this shape and never call connector_get.
func TestCompileForAddon_DeclaringAConnectorDoesNotGrantReadingIt(t *testing.T) {
	caps, access := security.CompileForAddon("link_inbox", manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "http:fetch", Target: "*.asteby.com"}},
		Connectors:   []manifest.ConnectorDef{{Key: "channel_gateway"}},
	})
	if err := caps.CanReadConnector("channel_gateway"); err == nil {
		t.Fatal("defining a connector must not authorise reading its credentials")
	}
	if len(access.Granted) != 0 {
		t.Fatalf("nothing may be granted without an explicit capability: %v", access.Granted)
	}
	if len(access.Implicit) != 0 {
		t.Fatalf("the implicit grant is retired and must stay empty: %v", access.Implicit)
	}
}

// The three addons that really call connector_get must keep reaching the gate
// on their explicit declarations, or CFDI stamping, Carta Porte stamping and
// GitHub sync break on the same deploy.
func TestCompileForAddon_RealConnectorGetCallersStayAuthorised(t *testing.T) {
	for _, tc := range []struct{ addon, kind, connector string }{
		{"fiscal_mexico", "connector:read", "factura_com"},
		{"waybill_cartaporte", "secrets:read", "factura_com"},
		{"integration_github", "connector:read", "github"},
	} {
		t.Run(tc.addon, func(t *testing.T) {
			caps, access := security.CompileForAddon(tc.addon, manifest.Manifest{
				Capabilities: []manifest.Capability{{Kind: tc.kind, Target: tc.connector}},
				Connectors:   []manifest.ConnectorDef{{Key: tc.connector}},
			})
			if err := caps.CanReadConnector(tc.connector); err != nil {
				t.Fatalf("%s must keep reading %s: %v", tc.addon, tc.connector, err)
			}
			if len(access.Implicit) != 0 {
				t.Fatalf("must not rely on the retired implicit grant: %v", access.Implicit)
			}
		})
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
