package manifest

import (
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest/v3"
)

// Model.extends, searchable and search_keys reach the DDL plane.
func TestFromV3Extends(t *testing.T) {
	m := &v3.Manifest{Models: []v3.Model{{
		Key: "TireSpec", Table: "product_tire_specs", Extends: "products.Product",
		Columns: []v3.Column{
			{Name: "section_width_mm", Type: "integer", Searchable: true},
			{Name: "aspect_ratio", Type: "integer"},
			{Name: "rim_diameter_in", Type: "numeric"},
		},
		SearchKeys: []v3.SearchKey{{Name: "size_key", Format: "{section_width_mm}/{aspect_ratio}R{rim_diameter_in}"}},
	}}}
	def := FromV3(m).ModelDefinitions[0]
	if def.Extends != "products.Product" || !def.OrgScoped {
		t.Fatalf("extends/org scope not carried: %+v", def)
	}
	byName := map[string]ColumnDef{}
	for _, c := range def.Columns {
		byName[c.Name] = c
	}
	if !byName["section_width_mm"].Index || byName["aspect_ratio"].Index {
		t.Fatalf("searchable must index only its column: %+v", def.Columns)
	}
	key, ok := byName["size_key"]
	if !ok || key.SearchKey == nil || !key.Index || !key.Readonly || key.Type != "text" {
		t.Fatalf("search key column: %+v", key)
	}
	want := []SearchKeyPart{
		{Column: "section_width_mm", Type: "integer"}, {Literal: "/"},
		{Column: "aspect_ratio", Type: "integer"}, {Literal: "R"},
		{Column: "rim_diameter_in", Type: "numeric"},
	}
	if !reflect.DeepEqual(key.SearchKey.Parts, want) {
		t.Fatalf("parts = %+v", key.SearchKey.Parts)
	}
}

func TestFromV3AttributeClasses(t *testing.T) {
	m := &v3.Manifest{Models: []v3.Model{{
		Key: "TireSpec", Table: "product_tire_specs", Extends: "products.Product",
		Columns: []v3.Column{{Name: "section_width_mm", Type: "integer", VisibleWhen: &v3.VisibleWhen{Class: "tire"}}},
		AttributeClasses: []v3.AttributeClass{{Key: "tire", Label: "Llanta", Sections: []v3.AttributeClassSection{
			{Key: "medida", Fields: []string{"section_width_mm"}},
		}}},
	}}}
	def := FromV3(m).ModelDefinitions[0]
	if len(def.AttributeClasses) != 1 || def.AttributeClasses[0].Sections[0].Fields[0] != "section_width_mm" {
		t.Fatalf("classes = %+v", def.AttributeClasses)
	}
	if def.Columns[0].VisibleWhen == nil || def.Columns[0].VisibleWhen.Class != "tire" {
		t.Fatalf("visible_when.class lost: %+v", def.Columns[0].VisibleWhen)
	}
}
