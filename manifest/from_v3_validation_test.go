package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const columnValidationManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "valfix", "name": "Val", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [{
    "key": "Item",
    "table": "val_items",
    "label": "Items",
    "columns": [
      { "name": "id", "type": "uuid", "primary_key": true },
      { "name": "organization_id", "type": "uuid", "not_null": true },
      { "name": "sku", "type": "text", "not_null": true, "validation": { "min": 3, "regex": "^[A-Z]+$" } }
    ]
  }],
  "contributions": {
    "actions": [{
      "key": "ship",
      "label": "Ship",
      "placement": "row",
      "target_model": "Item",
      "handler": { "type": "wasm", "function": "ship" },
      "fields": [
        { "key": "carrier", "label": "Carrier", "type": "text", "required": true, "validation": { "custom": "email" } }
      ]
    }]
  }
}`

func TestFromV3_ColumnAndActionValidation(t *testing.T) {
	m, err := v3.Parse([]byte(columnValidationManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	if m.Models[0].Columns[2].Validation == nil || m.Models[0].Columns[2].Validation.Min == nil {
		t.Fatal("v3 column.validation not decoded")
	}
	host := manifest.FromV3(m)
	var sku *manifest.ColumnDef
	for i := range host.ModelDefinitions[0].Columns {
		if host.ModelDefinitions[0].Columns[i].Name == "sku" {
			sku = &host.ModelDefinitions[0].Columns[i]
			break
		}
	}
	if sku == nil || sku.Validation == nil || sku.Validation.Min == nil || *sku.Validation.Min != 3 {
		t.Fatalf("column validation dropped: %+v", sku)
	}
	if sku.Validation.Regex != `^[A-Z]+$` {
		t.Fatalf("regex = %q", sku.Validation.Regex)
	}
	acts := host.Actions["Item"]
	if len(acts) == 0 || len(acts[0].Fields) == 0 || acts[0].Fields[0].Validation == nil {
		t.Fatalf("action field validation dropped: %+v", acts)
	}
	if acts[0].Fields[0].Validation.Custom != "email" {
		t.Fatalf("custom = %q", acts[0].Fields[0].Validation.Custom)
	}
}
