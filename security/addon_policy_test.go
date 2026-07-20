package security_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// The three addons that actually call connector_get in production, with their
// manifests exactly as published. Every one must keep resolving its connector,
// or GitHub sync / CFDI stamping / Carta Porte stamping break.
func TestCompileForAddon_ProductionManifests(t *testing.T) {
	cases := []struct {
		name      string
		addonKey  string
		m         manifest.Manifest
		connector string
		endpoint  string
		implicit  bool
	}{
		{
			name:     "fiscal_mexico owns factura_com, declares no capability",
			addonKey: "fiscal_mexico",
			m: manifest.Manifest{
				Capabilities: []manifest.Capability{{Kind: "http:fetch", Target: "api.factura.com"}},
				Connectors:   []manifest.ConnectorDef{{Key: "factura_com"}},
			},
			connector: "factura_com",
			endpoint:  "https://api.factura.com/cfdi40",
			implicit:  true,
		},
		{
			name:     "integration_github owns github, declares no capability",
			addonKey: "integration_github",
			m: manifest.Manifest{
				Capabilities: []manifest.Capability{{Kind: "http:fetch", Target: "api.github.com"}},
				Connectors:   []manifest.ConnectorDef{{Key: "github"}},
			},
			connector: "github",
			endpoint:  "https://api.github.com/repos/x/y",
			implicit:  true,
		},
		{
			name:     "waybill_cartaporte reuses factura_com via secrets:read",
			addonKey: "waybill_cartaporte",
			m: manifest.Manifest{
				Capabilities: []manifest.Capability{
					{Kind: "http:fetch", Target: "api.factura.com"},
					{Kind: "secrets:read", Target: "factura_com"},
				},
			},
			connector: "factura_com",
			endpoint:  "https://api.factura.com/timbrar",
			implicit:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps, implicit, refused := security.CompileForAddon(tc.addonKey, tc.m)
			if err := caps.CanReadConnector(tc.connector); err != nil {
				t.Fatalf("must keep reading %s: %v", tc.connector, err)
			}
			if err := caps.CanFetch(tc.endpoint); err != nil {
				t.Fatalf("must keep reaching %s: %v", tc.endpoint, err)
			}
			if len(refused) != 0 {
				t.Fatalf("published manifest must not have refused grants: %v", refused)
			}
			if got := len(implicit) > 0; got != tc.implicit {
				t.Fatalf("implicit reliance = %v, want %v (%v)", got, tc.implicit, implicit)
			}
		})
	}
}

// The ops#870 regression, at the level of the rule itself.
func TestCompileForAddon_UndeclaredConnectorDenied(t *testing.T) {
	caps, _, _ := security.CompileForAddon("evil_addon", manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "db:read", Target: "addon_evil_addon.*"}},
	})
	if err := caps.CanReadConnector("factura_com"); err == nil {
		t.Fatal("an addon that declares nothing must not read another addon's connector")
	}
	if err := caps.CanFetch("https://api.factura.com/x"); err == nil {
		t.Fatal("an addon must not fetch a host it did not declare")
	}
}

// Both spellings must gate identically — that is what makes migrating a
// manifest from secrets:read to connector:read a no-op.
func TestCompileForAddon_KindsEquivalent(t *testing.T) {
	for _, kind := range []string{"secrets:read", "connector:read"} {
		caps, implicit, _ := security.CompileForAddon("waybill", manifest.Manifest{
			Capabilities: []manifest.Capability{{Kind: kind, Target: "factura_com"}},
		})
		if err := caps.CanReadConnector("factura_com"); err != nil {
			t.Fatalf("%s must grant connector access: %v", kind, err)
		}
		if err := caps.CanReadConnector("github"); err == nil {
			t.Fatalf("%s must not grant unrelated connectors", kind)
		}
		if len(implicit) != 0 {
			t.Fatalf("%s is an explicit declaration, not implicit: %v", kind, implicit)
		}
	}
}

// An explicit declaration by the owner must be equivalent to the implicit
// grant, so dropping the implicit rule later is a no-op for migrated manifests.
func TestCompileForAddon_OwnerExplicitIsEquivalentAndNotImplicit(t *testing.T) {
	caps, implicit, _ := security.CompileForAddon("fiscal_mexico", manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "connector:read", Target: "factura_com"}},
		Connectors:   []manifest.ConnectorDef{{Key: "factura_com"}},
	})
	if err := caps.CanReadConnector("factura_com"); err != nil {
		t.Fatalf("migrated manifest must keep working: %v", err)
	}
	if len(implicit) != 0 {
		t.Fatalf("a declared connector must not report implicit reliance: %v", implicit)
	}
}

// A manifest must not be able to grant itself the wildcard the host gave up.
func TestCompileForAddon_WildcardRefused(t *testing.T) {
	for _, kind := range []string{"secrets:read", "connector:read"} {
		caps, _, refused := security.CompileForAddon("greedy", manifest.Manifest{
			Capabilities: []manifest.Capability{{Kind: kind, Target: "*", Reason: "gimme"}},
		})
		if err := caps.CanReadConnector("factura_com"); err == nil {
			t.Fatalf("%s \"*\" must not grant every connector — that is ops#870", kind)
		}
		if len(refused) != 1 {
			t.Fatalf("the wildcard must be reported as refused, got %v", refused)
		}
	}
}

// Non-connector capabilities must pass through untouched.
func TestCompileForAddon_PassesThroughOtherKinds(t *testing.T) {
	caps, _, _ := security.CompileForAddon("billing", manifest.Manifest{
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
	// And the self-schema grant Compile adds is still there.
	if err := caps.CanWriteModel("addon_billing.invoices"); err != nil {
		t.Fatalf("self-schema grant must survive: %v", err)
	}
}
