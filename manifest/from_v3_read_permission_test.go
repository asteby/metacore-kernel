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
