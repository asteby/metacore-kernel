package v3

import (
	"strings"
	"testing"
)

type im = map[string]interface{}

// snapModels: a sale line with a product snapshot, a grant slot and the formula
// that reads them.
func snapModels() []interface{} {
	return []interface{}{
		im{"key": "SalesOrderItem", "table": "sales_order_items",
			"columns": []interface{}{
				im{"name": "organization_id", "type": "uuid", "not_null": true},
				im{"name": "product_id", "type": "uuid", "ref": "products"},
				im{"name": "quantity", "type": "numeric"},
				im{"name": "unit_price", "type": "numeric"},
				im{"name": "min_price_at_sale", "type": "numeric", "protected": true},
				im{"name": "unit_cost", "type": "numeric", "protected": true},
				im{"name": "price_authorized", "type": "numeric", "protected": true},
				im{"name": "price_shortfall", "type": "numeric", "protected": true},
				im{"name": "price_approval_id", "type": "uuid", "grant": im{
					"policy": "price_below_min", "sets": "price_authorized",
					"context_eq":  im{"product_id": "product_id"},
					"context_gte": im{"unit_shortfall": "min_price_at_sale - unit_price"},
				}},
			},
			"snapshots": []interface{}{
				im{"from": "product_id", "fields": im{"min_price": "min_price_at_sale", "cost_price": "unit_cost"}},
			},
			"formulas": []interface{}{
				im{"target": "price_shortfall", "expr": "(min_price_at_sale - unit_price) * (1 - price_authorized)"},
			},
		},
	}
}

func validateSnap(t *testing.T, mutate func(col map[string]interface{}, mod map[string]interface{})) error {
	t.Helper()
	m := baseValid()
	models := snapModels()
	mod := models[0].(im)
	if mutate != nil {
		mutate(nil, mod)
	}
	m["models"] = models
	return Validate(mustJSON(t, m))
}

func TestValidate_Snapshots_Valid(t *testing.T) {
	if err := validateSnap(t, nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidate_Snapshots_Rejections(t *testing.T) {
	cases := map[string]struct {
		mutate func(col map[string]interface{}, mod map[string]interface{})
		want   string
	}{
		"from no existe": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["snapshots"].([]interface{})[0].(im)["from"] = "nope"
		}, "is not a column of the model"},
		"from sin ref": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["snapshots"].([]interface{})[0].(im)["from"] = "quantity"
		}, "has no ref"},
		"destino no protegido": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["columns"].([]interface{})[5].(im)["protected"] = false
		}, "must be declared protected"},
		"destino inexistente": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["snapshots"].([]interface{})[0].(im)["fields"] = im{"min_price": "ghost"}
		}, "is not a column of the model"},
		"fields vacío": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["snapshots"].([]interface{})[0].(im)["fields"] = im{}
		}, "fields"},
		"destino repetido": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["snapshots"] = append(mod["snapshots"].([]interface{}),
				im{"from": "product_id", "fields": im{"cost_price": "unit_cost"}})
		}, "more than one snapshot"},
		"grant sets no protegido": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["columns"].([]interface{})[6].(im)["protected"] = false
		}, "must be declared protected"},
		"grant sin policy": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["columns"].([]interface{})[8].(im)["grant"].(im)["policy"] = ""
		}, "policy"},
		"grant context_eq a columna ajena": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["columns"].([]interface{})[8].(im)["grant"].(im)["context_eq"] = im{"product_id": "ghost"}
		}, "is not a column of the model"},
		"grant expr con columna ajena": {func(_ map[string]interface{}, mod map[string]interface{}) {
			mod["columns"].([]interface{})[8].(im)["grant"].(im)["context_gte"] = im{"x": "ghost + 1"}
		}, "context_gte"},
	}
	for name, c := range cases {
		err := validateSnap(t, c.mutate)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got: %v", name, c.want, err)
		}
	}
}
