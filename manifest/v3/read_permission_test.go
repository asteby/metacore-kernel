package v3

import (
	"strings"
	"testing"
)

func readPermManifest(perm, rbac string) []byte {
	col := `{"name":"cost","type":"numeric"}`
	if perm != "" {
		col = `{"name":"cost","type":"numeric","read_permission":"` + perm + `"}`
	}
	extra := `"models": [{"key":"Product","table":"products","columns":[
      {"name":"id","type":"uuid"},{"name":"organization_id","type":"uuid"},` + col + `]}]`
	if rbac != "" {
		extra += `, "rbac": {"permissions": [` + rbac + `]}`
	}
	return rbacIdxManifest(extra)
}

func TestReadPermission_Accepted(t *testing.T) {
	for name, raw := range map[string][]byte{
		"own key declared in rbac": readPermManifest("pos.product.view_cost", `{"key":"pos.product.view_cost"}`),
		"another addon's key":      readPermManifest("inventory.product.view_cost", ""),
		"host key":                 readPermManifest("products.view_cost", ""),
		"no gate":                  readPermManifest("", ""),
	} {
		m, err := Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "own key declared in rbac" && m.Models[0].Columns[2].ReadPermission != "pos.product.view_cost" {
			t.Fatalf("read_permission not decoded: %+v", m.Models[0].Columns[2])
		}
	}
}

func TestReadPermission_Rejected(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"not dotted":         {readPermManifest("viewcost", ""), "read_permission"},
		"uppercase":          {readPermManifest("Inventory.Product.ViewCost", ""), "read_permission"},
		"wildcard":           {readPermManifest("*", ""), "read_permission"},
		"own key undeclared": {readPermManifest("pos.product.view_cost", `{"key":"pos.product.read"}`), "not declared in rbac.permissions"},
		"own key, no rbac":   {readPermManifest("pos.product.view_cost", ""), "not declared in rbac.permissions"},
	} {
		_, err := Parse(tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

func TestReadPermission_ValidateDirect(t *testing.T) {
	m := &Manifest{Metadata: Metadata{Key: "pos"}}
	if errs := validateReadPermission(m, "models[0].columns[0]", "pos.x.y"); len(errs) != 1 {
		t.Fatalf("undeclared own key: %v", errs)
	}
	m.RBAC = &RBAC{Permissions: []PermissionDef{{Key: "pos.x.y"}}}
	if errs := validateReadPermission(m, "models[0].columns[0]", "pos.x.y"); len(errs) != 0 {
		t.Fatalf("declared own key: %v", errs)
	}
	if errs := validateReadPermission(m, "w", "posx.y"); len(errs) != 0 {
		t.Fatalf("prefix must match a whole segment: %v", errs)
	}
}
