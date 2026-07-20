package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func granted(a manifest.ConnectorAccess, want string) bool {
	for _, k := range a.Granted {
		if k == want {
			return true
		}
	}
	return false
}

// The three addons that actually call connector_get in production, with their
// manifests exactly as published. Every one must keep resolving its connector,
// or GitHub sync / CFDI stamping / Carta Porte stamping break.
func TestConnectorAccessFor_ProductionManifests(t *testing.T) {
	cases := []struct {
		name      string
		m         manifest.Manifest
		connector string
		implicit  bool
	}{
		{
			name: "fiscal_mexico owns factura_com, declares no capability",
			m: manifest.Manifest{
				Capabilities: []manifest.Capability{{Kind: "http:fetch", Target: "api.factura.com"}},
				Connectors:   []manifest.ConnectorDef{{Key: "factura_com"}},
			},
			connector: "factura_com",
			implicit:  true,
		},
		{
			name: "integration_github owns github, declares no capability",
			m: manifest.Manifest{
				Capabilities: []manifest.Capability{{Kind: "http:fetch", Target: "api.github.com"}},
				Connectors:   []manifest.ConnectorDef{{Key: "github"}},
			},
			connector: "github",
			implicit:  true,
		},
		{
			name: "waybill_cartaporte reuses factura_com via secrets:read",
			m: manifest.Manifest{
				Capabilities: []manifest.Capability{
					{Kind: "http:fetch", Target: "api.factura.com"},
					{Kind: "secrets:read", Target: "factura_com"},
				},
			},
			connector: "factura_com",
			implicit:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			access := manifest.ConnectorAccessFor(tc.m)
			if !granted(access, tc.connector) {
				t.Fatalf("must authorise %s, got %v", tc.connector, access.Granted)
			}
			if len(access.Refused) != 0 {
				t.Fatalf("published manifest must not have refused grants: %v", access.Refused)
			}
			if got := len(access.Implicit) > 0; got != tc.implicit {
				t.Fatalf("implicit reliance = %v, want %v (%v)", got, tc.implicit, access.Implicit)
			}
		})
	}
}

// The ops#870 regression, at the level of the rule itself.
func TestConnectorAccessFor_UndeclaredGrantsNothing(t *testing.T) {
	access := manifest.ConnectorAccessFor(manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "db:read", Target: "addon_evil.*"}},
	})
	if len(access.Granted) != 0 {
		t.Fatalf("an addon that declares nothing must be granted nothing, got %v", access.Granted)
	}
}

// Both spellings must be equivalent — that is what makes migrating a manifest
// from secrets:read to connector:read a no-op.
func TestConnectorAccessFor_KindsEquivalent(t *testing.T) {
	for _, kind := range []string{"secrets:read", "connector:read"} {
		access := manifest.ConnectorAccessFor(manifest.Manifest{
			Capabilities: []manifest.Capability{{Kind: kind, Target: "factura_com"}},
		})
		if !granted(access, "factura_com") {
			t.Fatalf("%s must authorise the connector, got %v", kind, access.Granted)
		}
		if granted(access, "github") {
			t.Fatalf("%s must not authorise unrelated connectors", kind)
		}
		if len(access.Implicit) != 0 {
			t.Fatalf("%s is an explicit declaration, not implicit: %v", kind, access.Implicit)
		}
	}
}

// An explicit declaration by the owner must be equivalent to the implicit
// grant, so dropping the implicit branch later is a no-op for migrated
// manifests — and must not be double-counted.
func TestConnectorAccessFor_OwnerExplicitIsNotImplicit(t *testing.T) {
	access := manifest.ConnectorAccessFor(manifest.Manifest{
		Capabilities: []manifest.Capability{{Kind: "connector:read", Target: "factura_com"}},
		Connectors:   []manifest.ConnectorDef{{Key: "factura_com"}},
	})
	if !granted(access, "factura_com") {
		t.Fatalf("migrated manifest must keep working, got %v", access.Granted)
	}
	if len(access.Implicit) != 0 {
		t.Fatalf("a declared connector must not report implicit reliance: %v", access.Implicit)
	}
	if len(access.Granted) != 1 {
		t.Fatalf("the key must not be granted twice: %v", access.Granted)
	}
}

// A manifest must not be able to grant itself the wildcard the host gave up.
func TestConnectorAccessFor_WildcardRefused(t *testing.T) {
	for _, kind := range []string{"secrets:read", "connector:read"} {
		access := manifest.ConnectorAccessFor(manifest.Manifest{
			Capabilities: []manifest.Capability{{Kind: kind, Target: "*", Reason: "gimme"}},
		})
		if len(access.Granted) != 0 {
			t.Fatalf("%s \"*\" must authorise nothing — that is ops#870, got %v", kind, access.Granted)
		}
		if len(access.Refused) != 1 {
			t.Fatalf("the wildcard must be reported as refused, got %v", access.Refused)
		}
	}
}
