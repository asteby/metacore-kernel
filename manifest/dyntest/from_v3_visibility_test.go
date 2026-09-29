package dyntest

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// visibilityManifestJSON declares a line model whose money authority lives in
// minor units next to its currency mirror: the cents columns are internal
// ("list": API payloads only), the rest shows everywhere, plus one column per
// remaining scope.
const visibilityManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "shop", "name": "Shop", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "OrderLine",
      "table": "order_lines",
      "label": "Lines",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "unit_price", "type": "numeric" },
        { "name": "unit_price_cents", "type": "bigint", "visibility": "list" },
        { "name": "sku", "type": "text", "visibility": "table" },
        { "name": "note", "type": "text", "visibility": "modal" },
        { "name": "title", "type": "text", "visibility": "all" }
      ]
    }
  ]
}`

// QA 0927: a v3 column can be scoped out of the table/form like the legacy
// ColumnDef.visibility. Walks v3.Parse → FromV3 → legacy Validate →
// DeriveTableColumns / DeriveFormFields.
func TestColumnVisibilityParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(visibilityManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected column visibility: %v", err)
	}
	host := manifest.FromV3(m)
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "OrderLine" {
			def = d
		}
	}

	want := map[string]string{"unit_price": "", "unit_price_cents": "list", "sku": "table", "note": "modal", "title": "all"}
	served := map[string]string{}
	for _, c := range dynamic.DeriveTableColumns(def) {
		served[c.Key] = c.Visibility
	}
	for k, v := range want {
		if got, ok := served[k]; !ok || got != v {
			t.Errorf("table column %s visibility = %q (present=%v), want %q", k, got, ok, v)
		}
	}

	form := map[string]bool{}
	for _, f := range dynamic.DeriveFormFields(def) {
		form[f.Key] = true
	}
	for _, k := range []string{"unit_price", "note", "title"} {
		if !form[k] {
			t.Errorf("form must keep %s", k)
		}
	}
	for _, k := range []string{"unit_price_cents", "sku"} {
		if form[k] {
			t.Errorf("form must drop %s (visibility %s)", k, want[k])
		}
	}
}

func TestColumnVisibilityRejectsUnknownValue(t *testing.T) {
	bad := strings.Replace(visibilityManifestJSON, `"visibility": "list"`, `"visibility": "hidden"`, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted visibility \"hidden\"; want the closed set all|table|modal|list")
	}
}
