package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Screen nav may declare requires_capabilities so hosts expand Acceder grants
// with the API caps the federated UI needs (without hardcoding per addon).
const requiresCapabilitiesManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "POS", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "navigation": [
      {
        "title": "POS",
        "items": [
          {
            "title": "Terminal",
            "url": "addon://pos/terminal",
            "requires_capabilities": ["product.index", "salesorder.create"]
          }
        ]
      }
    ]
  }
}`

func TestParse_RequiresCapabilities_Accepted(t *testing.T) {
	m, err := v3.Parse([]byte(requiresCapabilitiesManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected requires_capabilities: %v", err)
	}
	if m.Contributions == nil || len(m.Contributions.Navigation) == 0 {
		t.Fatal("missing navigation")
	}
	it := m.Contributions.Navigation[0].Items[0]
	if len(it.RequiresCapabilities) != 2 {
		t.Fatalf("RequiresCapabilities = %#v", it.RequiresCapabilities)
	}
	if it.RequiresCapabilities[0] != "product.index" {
		t.Fatalf("first cap = %q", it.RequiresCapabilities[0])
	}

	legacy := manifest.FromV3(m)
	got := legacy.Navigation[0].Items[0].RequiresCapabilities
	if len(got) != 2 || got[0] != "product.index" {
		t.Fatalf("legacy RequiresCapabilities = %#v", got)
	}
}
