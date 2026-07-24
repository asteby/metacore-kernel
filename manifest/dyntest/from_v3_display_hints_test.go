package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// displayHintsManifestJSON declares a model whose columns carry v3 display
// hints (display / display_config / tooltip / description) plus columns that
// declare NO hint and should be auto-detected (url, creator, currency) by the
// derivation layer.
const displayHintsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "store", "name": "Store", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Product",
      "table": "addon_store_products",
      "label": "Products",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "name", "type": "text" },
        {
          "name": "homepage",
          "type": "text",
          "display": "url",
          "display_config": { "label_field": "name", "new_tab": true, "base_path": "https://example.com/" },
          "tooltip": "name",
          "description": "slug"
        },
        { "name": "website", "type": "text" },
        { "name": "created_by", "type": "uuid" },
        { "name": "price", "type": "numeric" },
        { "name": "sku", "type": "text" }
      ]
    }
  ]
}`

func TestDisplayHintsParseAndMap(t *testing.T) {
	m, err := v3.Parse([]byte(displayHintsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with display hints: %v", err)
	}

	// (a) the v3 typed shape carries the hints.
	var homepage *v3.Column
	for i := range m.Models[0].Columns {
		if m.Models[0].Columns[i].Name == "homepage" {
			homepage = &m.Models[0].Columns[i]
		}
	}
	if homepage == nil {
		t.Fatal("homepage column missing after parse")
	}
	if homepage.Display != "url" {
		t.Errorf("homepage.Display = %q, want url", homepage.Display)
	}
	if homepage.Tooltip != "name" || homepage.Description != "slug" {
		t.Errorf("homepage tooltip/description = %q/%q, want name/slug", homepage.Tooltip, homepage.Description)
	}
	if homepage.DisplayConfig["new_tab"] != true {
		t.Errorf("homepage.DisplayConfig[new_tab] = %v, want true", homepage.DisplayConfig["new_tab"])
	}

	// (b) FromV3 projects the hints onto the legacy ColumnDef carrier.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Product" {
			def = d
		}
	}
	cols := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		cols[c.Name] = c
	}
	hp := cols["homepage"]
	if hp.CellStyle != "url" {
		t.Errorf("legacy homepage.CellStyle = %q, want url", hp.CellStyle)
	}
	if hp.Tooltip != "name" || hp.Description != "slug" {
		t.Errorf("legacy homepage tooltip/description = %q/%q, want name/slug", hp.Tooltip, hp.Description)
	}
	if hp.StyleConfig["label_field"] != "name" {
		t.Errorf("legacy homepage.StyleConfig[label_field] = %v, want name", hp.StyleConfig["label_field"])
	}
	// base_path inside display_config is also projected onto BasePath.
	if hp.BasePath != "https://example.com/" {
		t.Errorf("legacy homepage.BasePath = %q, want https://example.com/", hp.BasePath)
	}

	// (c) auto-detect: columns with NO declared display get inferred cell
	// styles in the served metadata.
	served := dynamic.DeriveTableColumns(def)
	scell := map[string]string{}
	for _, c := range served {
		scell[c.Key] = c.CellStyle
	}
	if scell["homepage"] != "url" {
		t.Errorf("served homepage.CellStyle = %q, want url (declared)", scell["homepage"])
	}
	if scell["website"] != "url" {
		t.Errorf("served website.CellStyle = %q, want url (auto)", scell["website"])
	}
	if scell["created_by"] != "creator" {
		t.Errorf("served created_by.CellStyle = %q, want creator (auto)", scell["created_by"])
	}
	if scell["price"] != "currency" {
		t.Errorf("served price.CellStyle = %q, want currency (auto)", scell["price"])
	}
	// plain text columns stay unstyled.
	if scell["name"] != "" || scell["sku"] != "" {
		t.Errorf("served name/sku CellStyle = %q/%q, want empty", scell["name"], scell["sku"])
	}
	// served homepage also carries the projected tooltip/description/basePath.
	for _, c := range served {
		if c.Key == "homepage" {
			if c.Tooltip != "name" || c.Description != "slug" || c.BasePath != "https://example.com/" {
				t.Errorf("served homepage hints = %q/%q/%q", c.Tooltip, c.Description, c.BasePath)
			}
		}
	}
}
