package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// inlineManifest is a parent/child pair whose relation is built by the test.
func inlineManifest(rel manifest.RelationDef) manifest.Manifest {
	return manifest.Manifest{
		Key:     "inl",
		Name:    "Inl",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{
				TableName: "rules",
				ModelKey:  "Rule",
				Columns:   []manifest.ColumnDef{{Name: "name", Type: "string"}},
				Relations: []manifest.RelationDef{rel},
			},
			{
				TableName: "rule_tiers",
				ModelKey:  "RuleTier",
				Columns:   []manifest.ColumnDef{{Name: "rule_id", Type: "uuid"}, {Name: "percent", Type: "decimal"}},
			},
		},
	}
}

func inlineRel() manifest.RelationDef {
	return manifest.RelationDef{
		Name: "tiers", Kind: "one_to_many", Through: "RuleTier", ForeignKey: "rule_id",
		Embed: true, CreateInline: true, Min: 1,
	}
}

func TestValidate_InlineRelation_OK(t *testing.T) {
	m := inlineManifest(inlineRel())
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid create_inline relation should pass, got %v", err)
	}
}

func TestValidate_InlineRelation_Rules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *manifest.RelationDef)
		want   string
	}{
		{"requires embed", func(r *manifest.RelationDef) { r.Embed = false }, "create_inline requires embed"},
		{"incompatible with readonly", func(r *manifest.RelationDef) { r.Readonly = true }, "incompatible with readonly"},
		{"requires one_to_many", func(r *manifest.RelationDef) { r.Kind = "many_to_many"; r.Pivot = "rule_tiers_pivot" }, "requires kind one_to_many"},
		{"min needs create_inline", func(r *manifest.RelationDef) { r.CreateInline = false }, "min requires create_inline"},
		{"negative min", func(r *manifest.RelationDef) { r.Min = -1 }, "must be >= 0"},
		{"through must be a model", func(r *manifest.RelationDef) { r.Through = "Ghost" }, `through "Ghost" is not a model`},
		{"foreign_key must be a child column", func(r *manifest.RelationDef) { r.ForeignKey = "ghost_id" }, `foreign_key "ghost_id" is not a column`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := inlineRel()
			tc.mutate(&r)
			m := inlineManifest(r)
			err := m.Validate("2.0.0")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// A relation that declares neither create_inline nor min validates as before,
// whatever its other flags.
func TestValidate_InlineRelation_PlainRelationUnchanged(t *testing.T) {
	r := manifest.RelationDef{Name: "tiers", Kind: "one_to_many", Through: "Elsewhere", ForeignKey: "rule_id", Readonly: true}
	m := inlineManifest(r)
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("plain relation must keep validating, got %v", err)
	}
}

// With Addon the child model belongs to another manifest: the through/foreign
// key are not checked against this manifest's models (the kernel checks them
// when the child is written).
func TestValidate_InlineRelation_CrossAddonSkipsLocalModelCheck(t *testing.T) {
	r := inlineRel()
	r.Through = "PriceListItem"
	r.ForeignKey = "product_id"
	r.Addon = "customers"
	m := inlineManifest(r)
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("cross-addon create_inline must validate, got %v", err)
	}
	r.Addon = ""
	m = inlineManifest(r)
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatal("without addon a through outside the manifest must still be rejected")
	}
}
