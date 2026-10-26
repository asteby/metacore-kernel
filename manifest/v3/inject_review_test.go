package v3

import (
	"fmt"
	"testing"
)

// Review of the R2 schema package (#550): defects that would otherwise ship in
// the only manifest-schema bump of the extensibility plan.

// A filter is a fixed filter over a computed column: both must live on the same
// target list, otherwise the host would mount a filter over a column that list
// never shows.
func TestInjectReview_FilterMustShareTheColumnTarget(t *testing.T) {
	col := shippingStatusColumn() // target @commerce.order
	f := map[string]any{"key": "shipping_filter", "kind": "filter", "target": "customers.Customer",
		"filter": map[string]any{"column": "shipping_status"}}
	expectInjectErr(t, withInject(t, col, f), `targets "@commerce.order"`)

	f["target"] = "@commerce.order"
	if _, err := Parse(mustJSON(t, withInject(t, col, f))); err != nil {
		t.Fatalf("filter on the column's own target must validate: %v", err)
	}
}

// A filter must not point at itself or another filter: only a kind=column.
func TestInjectReview_FilterOverNonColumn(t *testing.T) {
	f := map[string]any{"key": "shipping_filter", "kind": "filter", "target": "@commerce.order",
		"filter": map[string]any{"column": "shipping_filter"}}
	expectInjectErr(t, withInject(t, f), "is not the key of a kind=column")
}

// "<own addon>.<Model>" is the recommended qualified form of an action target.
// It must point at a model the addon declares, and it must get the same
// own-model checks as the bare key (here: requires_state vs. stages).
func TestInjectReview_OwnQualifiedActionTarget(t *testing.T) {
	m := injectBase(t)
	a := buyLabelAction()
	a["target_model"] = "shipping.Shipmnt"
	obj(m["contributions"])["actions"] = []any{a}
	expectInjectErr(t, m, `"Shipmnt" is not one of its models`)

	m = injectBase(t)
	ship := obj(arr(m["models"])[0])
	ship["stage_field"] = "status"
	ship["stages"] = []any{
		map[string]any{"key": "draft", "label": "Draft"},
		map[string]any{"key": "sent", "label": "Sent"},
	}
	a = buyLabelAction()
	a["target_model"] = "shipping.Shipment"
	a["requires_state"] = []any{"bogus"}
	obj(m["contributions"])["actions"] = []any{a}
	expectInjectErr(t, m, `requires_state "bogus" is not a stage of model "Shipment"`)

	a["requires_state"] = []any{"draft"}
	if _, err := Parse(mustJSON(t, m)); err != nil {
		t.Fatalf("own qualified target with a valid stage must validate: %v", err)
	}
}

// column.where is equality on scalars (null = IS NULL): nested objects or
// arrays are neither meaningful nor bounded.
func TestInjectReview_ColumnWhereScalarsOnly(t *testing.T) {
	c := shippingStatusColumn()
	obj(c["column"])["where"] = map[string]any{"status": map[string]any{"$ne": map[string]any{"x": 1}}}
	expectInjectErr(t, withInject(t, c), "/contributions/inject/0/column/where/status")

	c = shippingStatusColumn()
	obj(c["column"])["where"] = map[string]any{"status": "delivered", "eta": nil, "cost": 0, "carrier": true}
	if _, err := Parse(mustJSON(t, withInject(t, c))); err != nil {
		t.Fatalf("scalar/null where must validate: %v", err)
	}
}

// Every computed column costs the host one grouped query per list page, so a
// hostile or careless manifest must not be able to declare thousands of them.
func TestInjectReview_InjectCountIsBounded(t *testing.T) {
	var in []any
	for i := 0; i <= MaxInjectPerAddon; i++ {
		c := shippingStatusColumn()
		c["key"] = fmt.Sprintf("col_%d", i)
		in = append(in, c)
	}
	expectInjectErr(t, withInject(t, in...), "/contributions/inject")
	errs := ValidateInject(InjectEnv{AddonKey: "shipping"}, make([]Inject, MaxInjectPerAddon+1), nil)
	found := false
	for _, e := range errs {
		if e == fmt.Sprintf("contributions.inject has %d entries; at most %d per addon", MaxInjectPerAddon+1, MaxInjectPerAddon) {
			found = true
		}
	}
	if !found {
		t.Fatalf("Go rule (legacy plane) must bound inject too, got %v", errs[:1])
	}
}

// An own-prefixed permission that rbac.permissions does not declare hides the
// contribution from every role (same rule as columns[].read_permission).
func TestInjectReview_OwnPermissionMustBeDeclared(t *testing.T) {
	p := shipmentPanel()
	p["permission"] = "shipping.shipmnt.read"
	m := withInject(t, p) // injectBase declares shipping.shipment.read
	expectInjectErr(t, m, `permission "shipping.shipmnt.read" is not declared in rbac.permissions`)

	p["permission"] = "shipping.shipment.read"
	if _, err := Parse(mustJSON(t, m)); err != nil {
		t.Fatalf("declared permission must validate: %v", err)
	}
	// A key of another addon (or the host) is resolved at runtime.
	p["permission"] = "customers.sales_order.read"
	if _, err := Parse(mustJSON(t, m)); err != nil {
		t.Fatalf("foreign permission must be shape-checked only: %v", err)
	}
}

// A negative order is refused by the schema too (what authors' editors read).
func TestInjectReview_SchemaRefusesNegativeOrder(t *testing.T) {
	p := shipmentPanel()
	p["order"] = -1
	expectInjectErr(t, withInject(t, p), "/contributions/inject/0/order")
}

// Model.import existed in Go but not in the schema (parity bug B1): R2 is the
// one bump where it can land, so a manifest can finally declare it.
func TestInjectReview_ModelImportIsInTheSchema(t *testing.T) {
	m := injectBase(t)
	ship := obj(arr(m["models"])[0])
	ship["import"] = map[string]any{
		"sheet_name": "Envios", "max_rows": 500, "instructions": []any{"No crea guias"},
		"columns": []any{map[string]any{"key": "tracking_number", "header": "Guia", "aliases": []any{"tracking"}, "required": true, "type": "string", "example": "1Z", "hint": "h"}},
	}
	if _, err := Parse(mustJSON(t, m)); err != nil {
		t.Fatalf("models[].import must validate: %v", err)
	}
	obj(arr(obj(ship["import"])["columns"])[0])["bogus"] = 1
	expectInjectErr(t, m, "/models/0/import/columns/0")
}
