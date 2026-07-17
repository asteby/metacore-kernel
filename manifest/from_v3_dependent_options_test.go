package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// dependentOptionsManifestJSON exercises the dependent-relational-options
// contract on BOTH planes: a model column and an action item_field carry
// depends_on + the `options` OBJECT form
// {source, filter_by, value, label_ref, description}. The transfer action's
// product_id picker is scoped by the header source_warehouse_id, lists candidates
// from the stock ledger, and resolves its label from the products model.
const dependentOptionsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "inventory", "name": "Inventory", "version": "0.8.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
  "models": [
    {
      "key": "StockTransferLine",
      "table": "addon_inventory_transfer_lines",
      "label": "Transfer Lines",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "warehouse_id", "type": "uuid" },
        {
          "name": "product_id",
          "type": "uuid",
          "depends_on": "warehouse_id",
          "options": {
            "source": "stock",
            "filter_by": "warehouse_id",
            "value": "product_id",
            "label": "product_name",
            "label_ref": "products.Product",
            "description": "quantity"
          }
        }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "transfer_stock",
        "label": "Transferir",
        "target_model": "StockTransfer",
        "handler": { "type": "compiled", "function": "OnTransfer" },
        "placement": "create",
        "fields": [
          { "key": "source_warehouse_id", "label": "Almacén origen", "type": "dynamic_select", "required": true, "ref": "Warehouse" },
          {
            "key": "items",
            "label": "Renglones",
            "type": "array",
            "required": true,
            "item_fields": [
              {
                "key": "product_id",
                "label": "Producto",
                "type": "dynamic_select",
                "required": true,
                "depends_on": "source_warehouse_id",
                "options": {
                  "source": "stock",
                  "filter_by": "warehouse_id",
                  "value": "product_id",
                  "label_ref": "products.Product",
                  "description": "quantity"
                }
              },
              { "key": "qty", "label": "Cantidad", "type": "number" }
            ]
          }
        ]
      }
    ]
  }
}`

func TestFromV3_DependentOptionsRoundTrip(t *testing.T) {
	m, err := v3.Parse([]byte(dependentOptionsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected dependent-options manifest: %v", err)
	}

	// (a) v3 typed shape: column carries depends_on + the dynamic options object.
	var col v3.Column
	for _, c := range m.Models[0].Columns {
		if c.Name == "product_id" {
			col = c
		}
	}
	if col.DependsOn != "warehouse_id" {
		t.Errorf("v3 column depends_on = %q, want warehouse_id", col.DependsOn)
	}
	if col.Options.Dynamic == nil {
		t.Fatalf("v3 column options did not decode as the dynamic object form")
	}
	if d := col.Options.Dynamic; d.Label != "product_name" {
		t.Errorf("column dynamic options Label = %q, want product_name", d.Label)
	}
	if d := col.Options.Dynamic; d.Source != "stock" || d.FilterBy != "warehouse_id" ||
		d.Value != "product_id" || d.LabelRef != "products.Product" || d.Description != "quantity" {
		t.Errorf("v3 column dynamic options = %+v", d)
	}

	// (b) v3 typed shape: the action item_field carries depends_on + the object.
	action := m.Contributions.Actions[0]
	items := action.Fields[1]
	if items.Key != "items" || len(items.ItemFields) != 2 {
		t.Fatalf("items field = %+v", items)
	}
	prod := items.ItemFields[0]
	if prod.DependsOn != "source_warehouse_id" {
		t.Errorf("v3 item_field depends_on = %q, want source_warehouse_id", prod.DependsOn)
	}
	if prod.Options.Dynamic == nil || prod.Options.Dynamic.LabelRef != "products.Product" {
		t.Errorf("v3 item_field dynamic options lost: %+v", prod.Options.Dynamic)
	}

	// (c) FromV3 projects both onto the legacy carriers.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "StockTransferLine" {
			def = d
		}
	}
	var legacyCol manifest.ColumnDef
	for _, c := range def.Columns {
		if c.Name == "product_id" {
			legacyCol = c
		}
	}
	if legacyCol.DependsOn != "warehouse_id" {
		t.Errorf("legacy column depends_on = %q, want warehouse_id", legacyCol.DependsOn)
	}
	if legacyCol.OptionsConfig == nil || legacyCol.OptionsConfig.Source != "stock" ||
		legacyCol.OptionsConfig.Label != "product_name" ||
		legacyCol.OptionsConfig.LabelRef != "products.Product" || legacyCol.OptionsConfig.Description != "quantity" {
		t.Errorf("legacy column OptionsConfig = %+v", legacyCol.OptionsConfig)
	}

	hostItems := host.Actions["StockTransfer"][0].Fields[1].ItemFields[0]
	if hostItems.DependsOn != "source_warehouse_id" {
		t.Errorf("legacy item_field depends_on = %q, want source_warehouse_id", hostItems.DependsOn)
	}
	if hostItems.OptionsConfig == nil || hostItems.OptionsConfig.FilterBy != "warehouse_id" {
		t.Fatalf("legacy item_field OptionsConfig lost: %+v", hostItems.OptionsConfig)
	}

	// (d) DeriveFormFields projects depends_on + a "dynamic" FieldOptionsConfig
	// onto the served column form field, and renders it as dynamic_select.
	var formProduct modelbase.FieldDef
	for _, f := range dynamic.DeriveFormFields(def) {
		if f.Key == "product_id" {
			formProduct = f
		}
	}
	if formProduct.Type != "dynamic_select" {
		t.Errorf("served form product_id.Type = %q, want dynamic_select", formProduct.Type)
	}
	if formProduct.DependsOn != "warehouse_id" {
		t.Errorf("served form product_id.DependsOn = %q, want warehouse_id", formProduct.DependsOn)
	}
	if formProduct.OptionsConfig == nil || formProduct.OptionsConfig.Type != "dynamic" ||
		formProduct.OptionsConfig.LabelRef != "products.Product" {
		t.Errorf("served form product_id.OptionsConfig = %+v", formProduct.OptionsConfig)
	}

	// (e) SERVED action spec: the JSON path ops uses (KernelManifestToRecord →
	// modelbase.ActionDef) preserves depends_on + the optionsConfig (with
	// type:"dynamic" so the kernel's Service.Options takes the query branch).
	raw, err := json.Marshal(host.Actions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var served map[string][]modelbase.ActionDef
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatalf("unmarshal into modelbase: %v", err)
	}
	servedItem := served["StockTransfer"][0].Fields[1].ItemFields[0]
	if servedItem.Key != "product_id" || servedItem.DependsOn != "source_warehouse_id" {
		t.Errorf("served action item_field key/depends_on = %q/%q", servedItem.Key, servedItem.DependsOn)
	}
	if servedItem.OptionsConfig == nil {
		t.Fatalf("served action item_field lost optionsConfig")
	}
	if oc := servedItem.OptionsConfig; oc.Type != "dynamic" || oc.Source != "stock" ||
		oc.FilterBy != "warehouse_id" || oc.Value != "product_id" ||
		oc.LabelRef != "products.Product" || oc.Description != "quantity" {
		t.Errorf("served action item_field OptionsConfig = %+v", oc)
	}

	// (f) the strict legacy validator accepts the converted manifest.
	if err := host.Validate("3.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected dependent-options manifest: %v", err)
	}
}

// TestV3DependentOptionsBothValidators proves a manifest carrying the new fields
// passes the LENIENT v3 schema validator AND the STRICT legacy validator — the
// two "double validation" planes the hub (strict) and manifestcheck (lenient)
// each run agree.
func TestV3DependentOptionsBothValidators(t *testing.T) {
	// Lenient v3 jsonschema + cross-field plane.
	if err := v3.Validate([]byte(dependentOptionsManifestJSON)); err != nil {
		t.Fatalf("lenient v3.Validate rejected dependent-options manifest: %v", err)
	}
	// Strict legacy plane (the hub's publish-time validator) on the converted shape.
	m, err := v3.Parse([]byte(dependentOptionsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	converted := manifest.FromV3(m)
	if err := converted.Validate("3.0.0"); err != nil {
		t.Fatalf("strict legacy Validate rejected dependent-options manifest: %v", err)
	}

	// The static array form of options must STILL validate (retrocompat): a
	// field with an enum array, not the object, is unaffected by the oneOf.
	staticForm := strings.Replace(
		dependentOptionsManifestJSON,
		`"type": "number" }`,
		`"type": "text", "options": [ { "value": "a", "label": "A" } ] }`,
		1,
	)
	if err := v3.Validate([]byte(staticForm)); err != nil {
		t.Fatalf("static options array form regressed under the oneOf: %v", err)
	}
}
