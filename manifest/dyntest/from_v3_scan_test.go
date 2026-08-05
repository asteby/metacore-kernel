package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// scanManifestJSON declares a model whose `sku` column opts into camera barcode
// scan-to-fill (`scan: true`), so the create/edit form input gets a scan button.
const scanManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "products", "name": "Products", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Product",
      "table": "products",
      "label": "Products",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "name", "type": "text", "not_null": true },
        { "name": "sku", "type": "text", "scan": true }
      ]
    }
  ]
}`

// TestColumnScanParseAndProject walks the scan flag through the whole kernel-side
// chain: v3.Parse (strict jsonschema + typed decode) → FromV3 (legacy carrier) →
// legacy Validate → DeriveFormFields (served metadata). Both validation planes
// must accept it and the flag must land on the served form field.
func TestColumnScanParseAndProject(t *testing.T) {
	// (a) strict schema + typed decode accepts and carries scan.
	m, err := v3.Parse([]byte(scanManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with column scan: %v", err)
	}
	cols := map[string]v3.Column{}
	for _, c := range m.Models[0].Columns {
		cols[c.Name] = c
	}
	if !cols["sku"].Scan {
		t.Errorf("v3 sku.Scan = false, want true")
	}
	if cols["name"].Scan {
		t.Errorf("v3 name.Scan = true, want false")
	}

	// (b) FromV3 carries it onto the legacy ColumnDef; (c) legacy Validate agrees.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Product" {
			def = d
		}
	}
	legacy := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		legacy[c.Name] = c
	}
	if !legacy["sku"].Scan {
		t.Errorf("legacy sku.Scan = false, want true")
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// (d) DeriveFormFields projects scan onto the served form field.
	fields := map[string]bool{}
	for _, f := range dynamic.DeriveFormFields(def) {
		fields[f.Key] = f.Scan
	}
	if !fields["sku"] {
		t.Errorf("served form sku.Scan = false, want true")
	}
	if fields["name"] {
		t.Errorf("served form name.Scan = true, want false")
	}
}
