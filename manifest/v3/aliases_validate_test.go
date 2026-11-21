package v3

import (
	"strings"
	"testing"
)

func aliasModel(key, table string, aliases ...string) map[string]interface{} {
	m := map[string]interface{}{
		"key": key, "table": table,
		"columns": []interface{}{
			map[string]interface{}{"name": "id", "type": "uuid"},
			map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
		},
	}
	if aliases != nil {
		as := make([]interface{}, len(aliases))
		for i, a := range aliases {
			as[i] = a
		}
		m["aliases"] = as
	}
	return m
}

func aliasManifest(models ...map[string]interface{}) map[string]interface{} {
	m := baseValid()
	out := make([]interface{}, len(models))
	for i, x := range models {
		out[i] = x
	}
	m["models"] = out
	return m
}

func TestModelAliases_Validation(t *testing.T) {
	if err := Validate(mustJSON(t, aliasManifest(aliasModel("Order", "orders", "sale", "order-items")))); err != nil {
		t.Fatalf("valid aliases: %v", err)
	}
	cases := map[string]struct {
		models []map[string]interface{}
		want   string
	}{
		"repeats own table":     {[]map[string]interface{}{aliasModel("Order", "orders", "Orders")}, "repeats its own table"},
		"repeats own key":       {[]map[string]interface{}{aliasModel("Order", "orders", "order")}, "repeats its own key"},
		"same model duplicate":  {[]map[string]interface{}{aliasModel("Order", "orders", "sale", "Sale")}, "another alias of the same model"},
		"another model's table": {[]map[string]interface{}{aliasModel("Order", "orders"), aliasModel("Quote", "quotes", "orders")}, "the table of Order"},
		"another model's alias": {[]map[string]interface{}{aliasModel("Order", "orders", "sale"), aliasModel("Quote", "quotes", "SALE")}, "an alias of Order"},
		"bad slug":              {[]map[string]interface{}{aliasModel("Order", "orders", "9sale")}, "pattern"},
	}
	for name, c := range cases {
		err := Validate(mustJSON(t, aliasManifest(c.models...)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}
