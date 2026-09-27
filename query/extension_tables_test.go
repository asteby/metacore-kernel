package query

import (
	"strings"
	"testing"
)

func extBuilder() *Builder {
	return New(refMeta()).WithTableName("products").WithExtensions([]ExtensionTable{
		{
			Key: "TireSpec", Table: "addon_products_tires.product_tire_specs",
			Columns:    map[string]struct{}{"rim_diameter_in": {}, "size_key": {}},
			SearchKeys: []ExtensionSearchKey{{Column: "size_key", Normalize: strings.ToUpper}},
		},
		{Key: "bad key", Table: "x"},                   // unsafe key: dropped
		{Key: "Evil", Table: "x; DROP TABLE products"}, // unsafe table: dropped
	})
}

func TestApply_ExtensionFilter(t *testing.T) {
	sql := renderSQL(t, extBuilder().Apply(openDryDB(t).Model(&testRow{}), Params{
		RelationFilters: []RelationFilter{
			{Relation: "TireSpec", Field: "rim_diameter_in", Op: OpEq, Value: "16"},
			{Relation: "TireSpec", Field: "undeclared", Op: OpEq, Value: "1"},
			{Relation: "Evil", Field: "rim_diameter_in", Op: OpEq, Value: "1"},
		},
	}))
	want := "products.id IN (SELECT __ex.id FROM addon_products_tires.product_tire_specs __ex WHERE __ex.rim_diameter_in = "
	if !strings.Contains(sql, want) {
		t.Fatalf("want %q in %q", want, sql)
	}
	if strings.Contains(sql, "undeclared") || strings.Contains(sql, "DROP") || strings.Count(sql, "__ex") != 3 {
		t.Fatalf("emitted a filter it must not: %q", sql)
	}
}

func TestApply_ExtensionSearchKey(t *testing.T) {
	sql := renderSQL(t, extBuilder().Apply(openDryDB(t).Model(&testRow{}), Params{Search: "205/55r16"}))
	for _, want := range []string{
		"number ILIKE",
		"OR products.id IN (SELECT __ex.id FROM addon_products_tires.product_tire_specs __ex WHERE __ex.size_key = ",
		"205/55R16",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("want %q in %q", want, sql)
		}
	}
}

func TestRegisterJSONBBag(t *testing.T) {
	if _, _, ok := isJSONBPathColumn("mx_specs.color"); ok {
		t.Fatal("unregistered bag must stay a relation")
	}
	RegisterJSONBBag("mx_specs")
	RegisterJSONBBag("bad bag")
	if bag, key, ok := isJSONBPathColumn("mx_specs.color"); !ok || bag != "mx_specs" || key != "color" {
		t.Fatalf("registered bag: %q %q %v", bag, key, ok)
	}
	if _, _, ok := isJSONBPathColumn("bad bag.x"); ok {
		t.Fatal("unsafe bag must not register")
	}
}
