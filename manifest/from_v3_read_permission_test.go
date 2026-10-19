package manifest

import (
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const readPermissionManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "inventory", "name": "Inventory", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Product",
      "table": "products",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "cost", "type": "numeric", "read_permission": "inventory.product.view_cost" }
      ]
    }
  ],
  "rbac": { "permissions": [{ "key": "inventory.product.view_cost" }] }
}`

// read_permission survives the v3 → host conversion onto ColumnDef.
func TestFromV3_ReadPermissionRidesThrough(t *testing.T) {
	m, err := v3.Parse([]byte(readPermissionManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := FromV3(m)
	for _, md := range host.ModelDefinitions {
		for _, c := range md.Columns {
			if c.Name == "cost" {
				if c.ReadPermission != "inventory.product.view_cost" {
					t.Fatalf("cost.ReadPermission = %q", c.ReadPermission)
				}
				return
			}
		}
	}
	t.Fatal("cost column not converted")
}

// The legacy (strict) plane enforces the same key shape as the v3 schema.
func TestValidateColumnExtensions_ReadPermission(t *testing.T) {
	if err := validateColumnExtensions(ColumnDef{Name: "cost", ReadPermission: "inventory.product.view_cost"}); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, bad := range []string{"cost", "*", "Inventory.Cost", "inventory..cost"} {
		err := validateColumnExtensions(ColumnDef{Name: "cost", ReadPermission: bad})
		if err == nil || !strings.Contains(err.Error(), "read_permission") {
			t.Fatalf("%q: err %v", bad, err)
		}
	}
}

// The legacy plane applies the derived-column rule too.
func TestValidateDerivedReadPermissions_Legacy(t *testing.T) {
	product := ModelDefinition{TableName: "products", ModelKey: "Product", Columns: []ColumnDef{
		{Name: "price"}, {Name: "cost", ReadPermission: "inv.product.view_cost"}, {Name: "margin"},
	}}
	line := ModelDefinition{TableName: "lines", ModelKey: "Line", Columns: []ColumnDef{
		{Name: "product_id", Ref: "inventory.Product"}, {Name: "unit_cost"},
	}}
	all := []ModelDefinition{product, line}

	p := product
	p.Formulas = []Formula{{Target: "margin", Expr: "price - cost"}}
	if err := validateDerivedReadPermissions(p, all); err == nil || !strings.Contains(err.Error(), "read_permission") {
		t.Fatalf("formula: %v", err)
	}
	p.Columns = []ColumnDef{{Name: "price"}, {Name: "cost", ReadPermission: "inv.product.view_cost"}, {Name: "margin", ReadPermission: "inv.product.view_cost"}}
	if err := validateDerivedReadPermissions(p, all); err != nil {
		t.Fatalf("gated formula target: %v", err)
	}

	l := line
	l.Snapshots = []SnapshotDef{{From: "product_id", Fields: map[string]string{"cost": "unit_cost"}}}
	if err := validateDerivedReadPermissions(l, all); err == nil || !strings.Contains(err.Error(), "snapshots[0]") {
		t.Fatalf("snapshot of a gated column: %v", err)
	}
}
