package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestProjectNavRequires(t *testing.T) {
	got := v3.ProjectNavRequires(
		[]v3.NavRequire{
			{Model: "Product", Actions: []string{"index"}},
			{Model: "customers.SalesOrder", Actions: []string{"index", "create"}},
		},
		[]string{"sales_returns.confirm_return", "product.index"},
	)
	want := []string{
		"product.index",
		"salesorder.index",
		"salesorder.create",
		"sales_returns.confirm_return",
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q (full %#v)", i, got[i], want[i], got)
		}
	}
}

const structuredRequiresManifestJSON = `{
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
            "requires": [
              { "model": "Product", "actions": ["index"] },
              { "model": "SalesOrder", "actions": ["create"] }
            ],
            "requires_capabilities": ["sales_returns.confirm_return"]
          }
        ]
      }
    ]
  }
}`

func TestParse_StructuredRequires_Projected(t *testing.T) {
	m, err := v3.Parse([]byte(structuredRequiresManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	got := legacy.Navigation[0].Items[0].RequiresCapabilities
	want := []string{"product.index", "salesorder.create", "sales_returns.confirm_return"}
	if len(got) != len(want) {
		t.Fatalf("RequiresCapabilities = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
