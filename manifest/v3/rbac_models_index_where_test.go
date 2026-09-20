package v3

import (
	"strings"
	"testing"
)

func rbacIdxManifest(extra string) []byte {
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "pos", "name": "POS", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "tenancy": {"isolation": "shared"},
      ` + extra + `
    }`)
}

const customersModel = `"models": [{"key":"Customer","table":"customers","columns":[
  {"name":"id","type":"uuid"},{"name":"organization_id","type":"uuid"},
  {"name":"external_id","type":"text"}]
  %s}],`

func TestPermissionModelsParse(t *testing.T) {
	raw := rbacIdxManifest(strings.Replace(customersModel, "%s", "", 1) + `
      "rbac": {"permissions": [
        {"key":"pos.sale.read","models":[{"model":"customers.SalesOrder","actions":["index","show"]}]},
        {"key":"pos.customer.read","models":[{"model":"Customer","actions":["index"]}]}
      ]}`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := m.RBAC.Permissions[0].Models[0]; got.Model != "customers.SalesOrder" || len(got.Actions) != 2 {
		t.Fatalf("models not decoded: %+v", got)
	}
}

func TestPermissionModelsRejected(t *testing.T) {
	for name, models := range map[string]string{
		"unknown own model": `[{"model":"Ghost","actions":["index"]}]`,
		"own key, unknown":  `[{"model":"pos.Ghost","actions":["index"]}]`,
		"no actions":        `[{"model":"Customer","actions":[]}]`,
		"bad action":        `[{"model":"Customer","actions":["Index!"]}]`,
		"empty model":       `[{"model":"","actions":["index"]}]`,
	} {
		raw := rbacIdxManifest(strings.Replace(customersModel, "%s", "", 1) +
			`"rbac": {"permissions": [{"key":"pos.x.read","models":` + models + `}]}`)
		if _, err := Parse(raw); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestIndexWhereParse(t *testing.T) {
	idx := `,"indices":[{"name":"customers_external_id_uq","columns":["external_id"],"unique":true,"where":"external_id IS NOT NULL AND external_id <> ''"}]`
	if _, err := Parse(rbacIdxManifest(strings.Replace(customersModel, "%s", idx, 1)+`"rbac":{}`)); err != nil {
		t.Fatalf("valid partial index rejected: %v", err)
	}
	for name, where := range map[string]string{
		"injection":      `external_id IS NOT NULL; DROP TABLE customers`,
		"unknown column": `ghost IS NOT NULL`,
		"subquery":       `external_id = (select 1)`,
	} {
		idx := `,"indices":[{"name":"x","columns":["external_id"],"unique":true,"where":"` + where + `"}]`
		if _, err := Parse(rbacIdxManifest(strings.Replace(customersModel, "%s", idx, 1)+`"rbac":{}`)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestParseIndexWhereRender(t *testing.T) {
	got, err := ParseIndexWhere("external_id is not null AND status != 'void' and n >= 2", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `"external_id" IS NOT NULL AND "status" <> 'void' AND "n" >= 2`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
