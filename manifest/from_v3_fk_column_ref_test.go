package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// fkColumnRefManifestJSON declares a model whose relations live at the MODEL
// level (foreign_keys) while the columns themselves carry NO explicit `ref`.
// This is the common authoring shape (e.g. inventory Stock → product_id /
// warehouse_id). FromV3 must DERIVE each FK column's ref from its
// references.model so the host resolves the relation's display name instead of
// rendering the raw UUID. An author-provided column ref still wins.
const fkColumnRefManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "inventory", "name": "Inventory", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Stock",
      "table": "stock",
      "label": "Stock",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "product_id", "type": "uuid" },
        { "name": "warehouse_id", "type": "uuid" },
        { "name": "supplier_id", "type": "uuid", "ref": "ExplicitSupplier" },
        { "name": "quantity", "type": "integer" }
      ],
      "foreign_keys": [
        { "columns": ["product_id"], "references": { "model": "inventory.Product", "columns": ["id"] }, "policy": "physical" },
        { "columns": ["warehouse_id"], "references": { "model": "inventory.Warehouse", "columns": ["id"] }, "policy": "physical" },
        { "columns": ["supplier_id"], "references": { "model": "inventory.Supplier", "columns": ["id"] }, "policy": "physical" }
      ]
    }
  ]
}`

func TestFromV3_DerivesColumnRefFromForeignKeys(t *testing.T) {
	m, err := v3.Parse([]byte(fkColumnRefManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with foreign_keys: %v", err)
	}

	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Stock" {
			def = d
		}
	}
	legacy := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		legacy[c.Name] = c
	}

	// FK columns with no explicit ref → ref derived from references.model.
	if legacy["product_id"].Ref != "inventory.Product" {
		t.Errorf("product_id.Ref = %q, want inventory.Product (derived from foreign_keys)", legacy["product_id"].Ref)
	}
	if legacy["warehouse_id"].Ref != "inventory.Warehouse" {
		t.Errorf("warehouse_id.Ref = %q, want inventory.Warehouse (derived from foreign_keys)", legacy["warehouse_id"].Ref)
	}
	// An author-provided column ref WINS over the FK target.
	if legacy["supplier_id"].Ref != "ExplicitSupplier" {
		t.Errorf("supplier_id.Ref = %q, want ExplicitSupplier (explicit ref wins)", legacy["supplier_id"].Ref)
	}
	// A non-FK column stays ref-less.
	if legacy["quantity"].Ref != "" {
		t.Errorf("quantity.Ref = %q, want empty (no FK)", legacy["quantity"].Ref)
	}

	// The derived ref flows through to the served column metadata as a
	// dynamic_select target, exactly like an explicit column ref.
	served := dynamic.DeriveTableColumns(def)
	ref := map[string]string{}
	for _, c := range served {
		ref[c.Key] = c.Ref
	}
	if ref["product_id"] != "inventory.Product" {
		t.Errorf("served product_id.Ref = %q, want inventory.Product", ref["product_id"])
	}
	if ref["warehouse_id"] != "inventory.Warehouse" {
		t.Errorf("served warehouse_id.Ref = %q, want inventory.Warehouse", ref["warehouse_id"])
	}
}
