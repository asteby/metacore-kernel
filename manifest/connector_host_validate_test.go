package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestValidate_HTTPFetchConnectorHost(t *testing.T) {
	mk := func(target string) manifest.Manifest {
		return salesManifest(func(m *manifest.Manifest) {
			m.Connectors = []manifest.ConnectorDef{{Key: "woocommerce", Credentials: []manifest.CredentialDef{{Key: "store_url"}}}}
			m.Capabilities = append(m.Capabilities, manifest.Capability{Kind: "http:fetch", Target: target})
		})
	}
	m := mk("connector:woocommerce.store_url")
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid connector host target rejected: %v", err)
	}
	for target, want := range map[string]string{
		"connector:woocommerce.ghost": "undeclared credential",
		"connector:woocommerce":       "must be",
		"connector:woo.*":             "must be",
	} {
		m := mk(target)
		if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", target, want, err)
		}
	}
}
