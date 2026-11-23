package v3

import (
	"strings"
	"testing"
)

// inlineModels is salesModels() with the parent's `items` relation declared as
// an inline-created composition (create_inline + embed), the shape
// docs/inline-relations.md describes. mutate edits the relation in place.
func inlineModels(mutate func(rel map[string]interface{})) []interface{} {
	models := salesModels()
	parent := models[0].(map[string]interface{})
	rel := parent["relations"].([]interface{})[0].(map[string]interface{})
	rel["embed"] = true
	rel["create_inline"] = true
	rel["min"] = 1
	if mutate != nil {
		mutate(rel)
	}
	return models
}

func validateInline(t *testing.T, mutate func(rel map[string]interface{})) error {
	t.Helper()
	m := baseValid()
	m["models"] = inlineModels(mutate)
	return Validate(mustJSON(t, m))
}

func TestValidate_InlineRelation_Valid(t *testing.T) {
	if err := validateInline(t, nil); err != nil {
		t.Fatalf("expected valid inline relation, got: %v", err)
	}
	// min es opcional y 0 es válido.
	if err := validateInline(t, func(r map[string]interface{}) { delete(r, "min") }); err != nil {
		t.Fatalf("create_inline without min: %v", err)
	}
	if err := validateInline(t, func(r map[string]interface{}) { r["min"] = 0 }); err != nil {
		t.Fatalf("min 0: %v", err)
	}
}

func TestValidate_InlineRelation_Rules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(rel map[string]interface{})
		want   string
	}{
		{"requires embed", func(r map[string]interface{}) { delete(r, "embed") }, "create_inline requires embed"},
		{"embed false", func(r map[string]interface{}) { r["embed"] = false }, "create_inline requires embed"},
		{"requires one_to_many", func(r map[string]interface{}) { r["kind"] = "many_to_many" }, "create_inline requires kind one_to_many"},
		{"incompatible with readonly", func(r map[string]interface{}) { r["readonly"] = true }, "incompatible with readonly"},
		{"min needs create_inline", func(r map[string]interface{}) { delete(r, "create_inline") }, "min requires create_inline"},
		{"through must be a model", func(r map[string]interface{}) { r["through"] = "Ghost" }, `through "Ghost" is not a model`},
		{"foreign_key must be a child column", func(r map[string]interface{}) { r["foreign_key"] = "ghost_id" }, `foreign_key "ghost_id" is not a column of model "SalesOrderItem"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInline(t, tc.mutate)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestValidate_InlineRelation_NegativeMinRejected(t *testing.T) {
	if err := validateInline(t, func(r map[string]interface{}) { r["min"] = -1 }); err == nil {
		t.Fatal("a negative min must be rejected (schema minimum 0 and Validate)")
	}
}

// Las relaciones existentes (sin create_inline ni min) no cambian: embed,
// readonly y through ajeno al manifiesto siguen válidos como antes.
func TestValidate_InlineRelation_ExistingRelationsUnchanged(t *testing.T) {
	err := validateInline(t, func(r map[string]interface{}) {
		delete(r, "create_inline")
		delete(r, "min")
		r["readonly"] = true
	})
	if err != nil {
		t.Fatalf("plain relation must keep validating: %v", err)
	}
}

func TestValidateInlineRelation_Unit(t *testing.T) {
	cols := map[string]map[string]struct{}{"Child": {"parent_id": {}}}
	ok := ModelRelation{Name: "rows", Kind: "one_to_many", Through: "Child", ForeignKey: "parent_id", Embed: true, CreateInline: true, Min: 2}
	if errs := validateInlineRelation("w", ok, cols, nil); len(errs) != 0 {
		t.Fatalf("valid relation: %v", errs)
	}
	bad := ok
	bad.Min = -1
	if errs := validateInlineRelation("w", bad, cols, nil); len(errs) != 1 || !strings.Contains(errs[0], "must be >= 0") {
		t.Fatalf("negative min: %v", errs)
	}
	plain := ModelRelation{Name: "rows", Kind: "one_to_many", Through: "Elsewhere", ForeignKey: "x"}
	if errs := validateInlineRelation("w", plain, cols, nil); len(errs) != 0 {
		t.Fatalf("a relation without create_inline is not checked: %v", errs)
	}
}

// validateInlineCross builds the inline relation pointing at ANOTHER addon's
// model (`addon` set, through not a model of this manifest); requireIt lists
// that addon in compatibility.requires.
func validateInlineCross(t *testing.T, requireIt bool, mutate func(rel map[string]interface{})) error {
	t.Helper()
	m := baseValid()
	m["models"] = inlineModels(func(r map[string]interface{}) {
		r["through"] = "PriceListItem"
		r["foreign_key"] = "product_id"
		r["addon"] = "customers"
		delete(r, "rollups") // a rollup needs the child columns, which another manifest owns
		if mutate != nil {
			mutate(r)
		}
	})
	if requireIt {
		c := m["compatibility"].(map[string]interface{})
		c["requires"] = append(c["requires"].([]interface{}), map[string]interface{}{"key": "customers", "version": ">=0.100.0"})
	}
	return Validate(mustJSON(t, m))
}

// Con `addon` el modelo hijo vive en otro manifiesto: no hace falta que sea de
// éste ni que se pueda comprobar su columna; sí que el addon esté en requires.
func TestValidate_InlineRelation_CrossAddon(t *testing.T) {
	if err := validateInlineCross(t, true, nil); err != nil {
		t.Fatalf("a relation to a required addon's model must validate: %v", err)
	}
	if err := validateInlineCross(t, true, func(r map[string]interface{}) { r["widget"] = "market_prices" }); err != nil {
		t.Fatalf("widget on a cross-addon relation: %v", err)
	}
	err := validateInlineCross(t, false, nil)
	if err == nil || !strings.Contains(err.Error(), `addon "customers" must be listed in compatibility.requires`) {
		t.Fatalf("addon not in requires must be rejected, got: %v", err)
	}
	// Sin `addon` el through ajeno sigue prohibido (comportamiento de siempre).
	err = validateInlineCross(t, true, func(r map[string]interface{}) { delete(r, "addon") })
	if err == nil || !strings.Contains(err.Error(), `through "PriceListItem" is not a model`) {
		t.Fatalf("without addon the through must be a model of the manifest, got: %v", err)
	}
	// Las demás reglas de create_inline siguen aplicando con addon.
	err = validateInlineCross(t, true, func(r map[string]interface{}) { r["embed"] = false })
	if err == nil || !strings.Contains(err.Error(), "create_inline requires embed") {
		t.Fatalf("create_inline rules still apply with addon, got: %v", err)
	}
}

func TestValidate_RelationWidgetKey(t *testing.T) {
	for _, bad := range []string{"Market Prices", "1abc", "a/b", "A"} {
		err := validateInline(t, func(r map[string]interface{}) { r["widget"] = bad })
		if err == nil {
			t.Fatalf("widget %q must be rejected", bad)
		}
	}
	for _, good := range []string{"market_prices", "products.market-prices"} {
		if err := validateInline(t, func(r map[string]interface{}) { r["widget"] = good }); err != nil {
			t.Fatalf("widget %q must validate: %v", good, err)
		}
	}
}

func TestValidateInlineRelation_UnitCrossAddon(t *testing.T) {
	rel := ModelRelation{Name: "prices", Kind: "one_to_many", Through: "PriceListItem", ForeignKey: "product_id", Embed: true, CreateInline: true, Addon: "customers"}
	if errs := validateInlineRelation("w", rel, map[string]map[string]struct{}{}, map[string]struct{}{"customers": {}}); len(errs) != 0 {
		t.Fatalf("cross-addon relation with the addon required: %v", errs)
	}
	if errs := validateInlineRelation("w", rel, map[string]map[string]struct{}{}, nil); len(errs) != 1 {
		t.Fatalf("cross-addon relation without the addon required: %v", errs)
	}
}
