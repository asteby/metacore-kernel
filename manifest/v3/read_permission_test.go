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

// derivedManifest builds an addon whose Product gates cost and declares extra
// model JSON (formulas, relations, ...) and extra columns.
func derivedManifest(extraCols, extraModel, extraModels string) []byte {
	return rbacIdxManifest(`"models": [{"key":"Product","table":"products","columns":[
      {"name":"id","type":"uuid"},{"name":"organization_id","type":"uuid"},
      {"name":"price","type":"numeric"},
      {"name":"cost","type":"numeric","read_permission":"pos.product.view_cost"}` + extraCols + `]` + extraModel + `}` + extraModels + `],
      "rbac": {"permissions": [{"key":"pos.product.view_cost"},{"key":"pos.product.view_other"}]}`)
}

func TestReadPermission_DerivedColumns(t *testing.T) {
	const gatedMargin = `,{"name":"margin","type":"numeric","read_permission":"pos.product.view_cost"}`
	const openMargin = `,{"name":"margin","type":"numeric"}`
	const otherMargin = `,{"name":"margin","type":"numeric","read_permission":"pos.product.view_other"}`
	const lines = `,{"key":"Line","table":"lines","columns":[
      {"name":"id","type":"uuid"},{"name":"organization_id","type":"uuid"},{"name":"product_id","type":"uuid"},
      {"name":"line_cost","type":"numeric","read_permission":"pos.product.view_cost"}]}`
	ok := map[string][]byte{
		"formula gated like its source": derivedManifest(gatedMargin, `,"formulas":[{"target":"margin","expr":"price - cost"}]`, ""),
		"formula over ungated columns":  derivedManifest(openMargin, `,"formulas":[{"target":"margin","expr":"price * 2"}]`, ""),
		"generated gated":               derivedManifest(`,{"name":"margin","type":"numeric","generated":"price - cost","read_permission":"pos.product.view_cost"}`, "", ""),
		"rollup gated": derivedManifest(`,{"name":"total_cost","type":"numeric","read_permission":"pos.product.view_cost"}`,
			`,"relations":[{"name":"lines","kind":"one_to_many","through":"Line","foreign_key":"product_id","rollups":[{"target":"total_cost","fn":"sum","from":"line_cost"}]}]`, lines),
	}
	for name, raw := range ok {
		if _, err := Parse(raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	bad := map[string][]byte{
		"formula margin = price - cost":   derivedManifest(openMargin, `,"formulas":[{"target":"margin","expr":"price - cost"}]`, ""),
		"formula with another permission": derivedManifest(otherMargin, `,"formulas":[{"target":"margin","expr":"price - cost"}]`, ""),
		"generated column":                derivedManifest(`,{"name":"margin","type":"numeric","generated":"price - cost"}`, "", ""),
		"rollup sum(lines.line_cost)": derivedManifest(`,{"name":"total_cost","type":"numeric"}`,
			`,"relations":[{"name":"lines","kind":"one_to_many","through":"Line","foreign_key":"product_id","rollups":[{"target":"total_cost","fn":"sum","from":"line_cost"}]}]`, lines),
		"rollup expr": derivedManifest(`,{"name":"total_cost","type":"numeric"}`,
			`,"relations":[{"name":"lines","kind":"one_to_many","through":"Line","foreign_key":"product_id","rollups":[{"target":"total_cost","fn":"sum","expr":"line_cost * 2"}]}]`, lines),
		"search key over cost": derivedManifest("", `,"search_keys":[{"name":"cost_key","format":"{cost}"}]`, ""),
	}
	for name, raw := range bad {
		_, err := Parse(raw)
		if err == nil || !strings.Contains(err.Error(), "read_permission") {
			t.Fatalf("%s: err %v, want a read_permission error", name, err)
		}
	}
}

func TestDerivedReadPermissionError(t *testing.T) {
	if msg := DerivedReadPermissionError("m", "", map[string]string{"price": ""}); msg != "" {
		t.Fatal(msg)
	}
	if msg := DerivedReadPermissionError("m", "a.b", map[string]string{"x": "a.b", "y": "c.d"}); msg == "" {
		t.Fatal("two different source permissions cannot feed one column")
	}
}
