package v3

import (
	"strings"
	"testing"
)

func writePermManifest(colExtra, rbac string) []byte {
	col := `{"name":"discount","type":"numeric"` + colExtra + `}`
	extra := `"models": [{"key":"Product","table":"products","columns":[
      {"name":"id","type":"uuid"},{"name":"organization_id","type":"uuid"},` + col + `]}]`
	if rbac != "" {
		extra += `, "rbac": {"permissions": [` + rbac + `]}`
	}
	return rbacIdxManifest(extra)
}

func TestWritePermission_Accepted(t *testing.T) {
	for name, raw := range map[string][]byte{
		"own key declared":  writePermManifest(`,"write_permission":"pos.product.set_discount"`, `{"key":"pos.product.set_discount"}`),
		"another addon key": writePermManifest(`,"write_permission":"billing.invoice.set_discount"`, ""),
		"no gate":           writePermManifest(``, ""),
		"with read gate":    writePermManifest(`,"read_permission":"billing.invoice.view","write_permission":"billing.invoice.set_discount"`, ""),
	} {
		m, err := Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "own key declared" && m.Models[0].Columns[2].WritePermission != "pos.product.set_discount" {
			t.Fatalf("write_permission not decoded: %+v", m.Models[0].Columns[2])
		}
	}
}

func TestWritePermission_Rejected(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"not dotted":         {writePermManifest(`,"write_permission":"setdiscount"`, ""), "write_permission"},
		"wildcard":           {writePermManifest(`,"write_permission":"*"`, ""), "write_permission"},
		"own key undeclared": {writePermManifest(`,"write_permission":"pos.product.set_discount"`, ""), "not declared in rbac.permissions"},
		"with protected":     {writePermManifest(`,"protected":true,"write_permission":"billing.invoice.set_discount"`, ""), "conflicts with protected"},
		"with generated":     {writePermManifest(`,"generated":"price * 2","write_permission":"billing.invoice.set_discount"`, ""), "generated"},
	} {
		_, err := Parse(tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}
