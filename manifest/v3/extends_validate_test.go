package v3

import (
	"strings"
	"testing"
)

// tireSpecModel is a 1:1 extension of products.Product as products_tires
// would declare it (CONTRACT-item-master.md §3.1): no id, no organization_id.
func tireSpecModel() map[string]interface{} {
	return map[string]interface{}{
		"key":     "TireSpec",
		"table":   "product_tire_specs",
		"extends": "products.Product",
		"columns": []interface{}{
			map[string]interface{}{"name": "section_width_mm", "type": "integer", "searchable": true},
			map[string]interface{}{"name": "aspect_ratio", "type": "integer", "searchable": true},
			map[string]interface{}{"name": "rim_diameter_in", "type": "numeric", "searchable": true},
			map[string]interface{}{"name": "speed_rating", "type": "text"},
		},
		"search_keys": []interface{}{
			map[string]interface{}{"name": "size_key", "format": "{section_width_mm}/{aspect_ratio}R{rim_diameter_in}"},
		},
	}
}

func withModels(models ...map[string]interface{}) map[string]interface{} {
	m := baseValid()
	items := make([]interface{}, 0, len(models))
	for _, mod := range models {
		items = append(items, mod)
	}
	m["models"] = items
	return m
}

func validateErr(t *testing.T, m map[string]interface{}) string {
	t.Helper()
	err := Validate(mustJSON(t, m))
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestExtends_ValidExtensionTable(t *testing.T) {
	if msg := validateErr(t, withModels(tireSpecModel())); msg != "" {
		t.Fatalf("expected valid, got %s", msg)
	}
	var m Manifest
	parsed, err := Parse(mustJSON(t, withModels(tireSpecModel())))
	if err != nil {
		t.Fatal(err)
	}
	m = *parsed
	mod := m.Models[0]
	if mod.Extends != "products.Product" || len(mod.SearchKeys) != 1 || !mod.Columns[0].Searchable {
		t.Fatalf("typed shape lost the new fields: %+v", mod)
	}
}

func TestExtends_RejectsInjectedColumns(t *testing.T) {
	mod := tireSpecModel()
	mod["columns"] = append(mod["columns"].([]interface{}),
		map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
		map[string]interface{}{"name": "organization_id", "type": "uuid"},
	)
	msg := validateErr(t, withModels(mod))
	for _, want := range []string{`must not declare "id"`, `must not declare "organization_id"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %s", want, msg)
		}
	}
}

func TestExtends_RejectsOwnLifecycle(t *testing.T) {
	mod := tireSpecModel()
	mod["sequences"] = []interface{}{map[string]interface{}{"key": "tire", "format": "T-{seq:05}"}}
	mod["relations"] = []interface{}{map[string]interface{}{"name": "x", "kind": "one_to_many", "through": "Other", "foreign_key": "spec_id"}}
	msg := validateErr(t, withModels(mod))
	if !strings.Contains(msg, "cannot declare sequences") || !strings.Contains(msg, "cannot declare relations") {
		t.Fatalf("got %s", msg)
	}
}

func TestExtends_TargetShape(t *testing.T) {
	mod := tireSpecModel()
	mod["extends"] = "Product"
	if msg := validateErr(t, withModels(mod)); msg == "" {
		t.Fatal("extends without addon namespace must fail")
	}
}

func TestExtends_SameAddonTarget(t *testing.T) {
	lot := map[string]interface{}{
		"key": "Lot", "table": "inventory_lots",
		"columns": []interface{}{
			map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
			map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
			map[string]interface{}{"name": "lot_number", "type": "text"},
		},
	}
	ext := map[string]interface{}{
		"key": "LotQuality", "table": "inventory_lot_quality", "extends": "inventory.Lot",
		"columns": []interface{}{map[string]interface{}{"name": "grade", "type": "text"}},
	}

	// Not opened to extension.
	if msg := validateErr(t, withModels(lot, ext)); !strings.Contains(msg, "model_extensions_accepted") {
		t.Fatalf("got %q", msg)
	}

	m := withModels(lot, ext)
	m["extension_points"] = map[string]interface{}{"model_extensions_accepted": []interface{}{"Lot"}}
	if msg := validateErr(t, m); msg != "" {
		t.Fatalf("expected valid, got %s", msg)
	}

	// Unknown same-addon target, self-extension and chains.
	ext["extends"] = "inventory.Missing"
	if msg := validateErr(t, withModels(lot, ext)); !strings.Contains(msg, "is not a model of this addon") {
		t.Fatalf("got %q", msg)
	}
	ext["extends"] = "inventory.LotQuality"
	if msg := validateErr(t, withModels(lot, ext)); !strings.Contains(msg, "cannot extend itself") {
		t.Fatalf("got %q", msg)
	}
	chained := map[string]interface{}{
		"key": "LotExtra", "table": "inventory_lot_extra", "extends": "inventory.LotQuality",
		"columns": []interface{}{map[string]interface{}{"name": "note", "type": "text"}},
	}
	ext["extends"] = "inventory.Lot"
	if msg := validateErr(t, withModels(lot, ext, chained)); !strings.Contains(msg, "is itself an extension") {
		t.Fatalf("got %q", msg)
	}
}

func TestSearchable_ScalarOnly(t *testing.T) {
	mod := tireSpecModel()
	mod["columns"] = append(mod["columns"].([]interface{}),
		map[string]interface{}{"name": "raw", "type": "jsonb", "searchable": true})
	if msg := validateErr(t, withModels(mod)); !strings.Contains(msg, "searchable is for scalar columns") {
		t.Fatalf("got %q", msg)
	}
}

func TestSearchKeyMatchModes(t *testing.T) {
	for _, match := range []string{"", "normalized", "normalized_prefix", "exact"} {
		mod := tireSpecModel()
		mod["search_keys"] = []interface{}{map[string]interface{}{"name": "size_key", "format": "{aspect_ratio}", "match": match}}
		if match == "" {
			delete(mod["search_keys"].([]interface{})[0].(map[string]interface{}), "match")
		}
		if err := Validate(mustJSON(t, withModels(mod))); err != nil {
			t.Errorf("match %q must be valid: %v", match, err)
		}
	}
	mod := tireSpecModel()
	mod["search_keys"] = []interface{}{map[string]interface{}{"name": "size_key", "format": "{aspect_ratio}", "match": "fuzzy"}}
	if err := Validate(mustJSON(t, withModels(mod))); err == nil {
		t.Error("unknown match mode must be refused")
	}
}

func TestSearchKeys(t *testing.T) {
	cases := []struct {
		name string
		keys []interface{}
		want string
	}{
		{"unknown placeholder", []interface{}{map[string]interface{}{"name": "size_key", "format": "{width}/{aspect_ratio}"}}, "{width} is not a declared column"},
		{"no placeholder", []interface{}{map[string]interface{}{"name": "size_key", "format": "R16"}}, "has no {column} placeholder"},
		{"collides with column", []interface{}{map[string]interface{}{"name": "speed_rating", "format": "{aspect_ratio}"}}, "collides with a declared column"},
		{"duplicate", []interface{}{
			map[string]interface{}{"name": "size_key", "format": "{aspect_ratio}"},
			map[string]interface{}{"name": "size_key", "format": "{rim_diameter_in}"},
		}, "duplicate name"},
		{"unbalanced", []interface{}{map[string]interface{}{"name": "size_key", "format": "{aspect_ratio}/{rim"}}, "unbalanced brace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod := tireSpecModel()
			mod["search_keys"] = tc.keys
			if msg := validateErr(t, withModels(mod)); !strings.Contains(msg, tc.want) {
				t.Fatalf("want %q, got %q", tc.want, msg)
			}
		})
	}
}

// The jsonb bag of a legacy ModelExtension is now part of the schema and the
// typed shape (hosts used to parse it from raw bytes and infer it otherwise).
func TestModelExtension_JSONBag(t *testing.T) {
	probe := map[string]interface{}{
		"key": "TireSpecProbe", "table": "products_tires_probe",
		"columns": []interface{}{
			map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
			map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
		},
		"extensions": []interface{}{map[string]interface{}{
			"target_model": "products.Product",
			"jsonb_bag":    "product_specs",
			"columns":      []interface{}{map[string]interface{}{"name": "dot", "type": "text"}},
		}},
	}
	raw := mustJSON(t, withModels(probe))
	if err := Validate(raw); err != nil {
		t.Fatalf("jsonb_bag must be accepted: %v", err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Models[0].Extensions[0].JSONBag; got != "product_specs" {
		t.Fatalf("JSONBag = %q", got)
	}
}

func tireClasses() []interface{} {
	return []interface{}{map[string]interface{}{
		"key": "tire",
		"sections": []interface{}{
			map[string]interface{}{"key": "medida", "fields": []interface{}{"section_width_mm", "aspect_ratio", "rim_diameter_in"}},
			map[string]interface{}{"key": "indices", "fields": []interface{}{"speed_rating"}},
		},
	}}
}

func TestAttributeClasses_Valid(t *testing.T) {
	mod := tireSpecModel()
	mod["attribute_classes"] = tireClasses()
	cols := mod["columns"].([]interface{})
	cols[0].(map[string]interface{})["visible_when"] = map[string]interface{}{"class": "tire"}
	raw := mustJSON(t, withModels(mod))
	if err := Validate(raw); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Models[0].AttributeClasses[0].Key != "tire" || m.Models[0].Columns[0].VisibleWhen.Class != "tire" {
		t.Fatalf("typed shape lost the classes: %+v", m.Models[0])
	}
}

func TestAttributeClasses_Rejects(t *testing.T) {
	onPlain := map[string]interface{}{
		"key": "Plain", "table": "plains",
		"columns": []interface{}{
			map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
			map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
			map[string]interface{}{"name": "grade", "type": "text"},
		},
		"attribute_classes": []interface{}{map[string]interface{}{"key": "x", "sections": []interface{}{map[string]interface{}{"key": "s", "fields": []interface{}{"grade"}}}}},
	}
	if msg := validateErr(t, withModels(onPlain)); !strings.Contains(msg, "only a model with extends") {
		t.Fatalf("got %q", msg)
	}

	mod := tireSpecModel()
	classes := tireClasses()
	classes[0].(map[string]interface{})["sections"].([]interface{})[0].(map[string]interface{})["fields"] = []interface{}{"nope"}
	mod["attribute_classes"] = classes
	if msg := validateErr(t, withModels(mod)); !strings.Contains(msg, `"nope" is not a column`) {
		t.Fatalf("got %q", msg)
	}

	mod = tireSpecModel()
	mod["columns"].([]interface{})[0].(map[string]interface{})["visible_when"] = map[string]interface{}{"class": "moto"}
	if msg := validateErr(t, withModels(mod)); !strings.Contains(msg, `"moto" is not declared`) {
		t.Fatalf("got %q", msg)
	}

	mod = tireSpecModel()
	mod["attribute_classes"] = tireClasses()
	mod["columns"].([]interface{})[0].(map[string]interface{})["visible_when"] = map[string]interface{}{"class": "tire", "field": "x", "equals": "y"}
	if msg := validateErr(t, withModels(mod)); !strings.Contains(msg, "class is used alone") {
		t.Fatalf("got %q", msg)
	}
}
