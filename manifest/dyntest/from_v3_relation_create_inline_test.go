package dyntest

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// inlineManifestJSON: a DiscountRule whose `tiers` are captured together with
// the rule (create_inline, at least one tier), the motivating case of
// docs/inline-relations.md.
const inlineManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "customers", "name": "Customers", "version": "1.0.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "DiscountRule",
      "table": "discount_rules",
      "label": "Discount rules",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "name", "type": "text" }
      ],
      "relations": [
        {
          "name": "tiers",
          "kind": "one_to_many",
          "through": "DiscountRuleTier",
          "foreign_key": "discount_rule_id",
          "embed": true,
          "create_inline": true,
          "min": 1
        }
      ]
    },
    {
      "key": "DiscountRuleTier",
      "table": "discount_rule_tiers",
      "label": "Tiers",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "discount_rule_id", "type": "uuid" }
      ]
    }
  ]
}`

// TestRelationCreateInlineParseAndProject walks create_inline / min through the
// kernel chain: strict v3 schema + typed decode → FromV3 → legacy Validate.
func TestRelationCreateInlineParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(inlineManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected create_inline relation: %v", err)
	}
	r := m.Models[0].Relations[0]
	if !r.CreateInline || r.Min != 1 {
		t.Fatalf("v3 relation = %+v, want create_inline and min 1", r)
	}

	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "DiscountRule" {
			def = d
		}
	}
	if len(def.Relations) != 1 || !def.Relations[0].CreateInline || def.Relations[0].Min != 1 {
		t.Fatalf("legacy relations = %+v, want create_inline and min 1", def.Relations)
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// The legacy surface enforces the same rules once the flag is wrong.
	for name, mutate := range map[string]func(r *manifest.RelationDef){
		"no embed":     func(r *manifest.RelationDef) { r.Embed = false },
		"readonly":     func(r *manifest.RelationDef) { r.Readonly = true },
		"ghost child":  func(r *manifest.RelationDef) { r.Through = "Ghost" },
		"bad fk":       func(r *manifest.RelationDef) { r.ForeignKey = "ghost_id" },
		"negative min": func(r *manifest.RelationDef) { r.Min = -1 },
	} {
		bad := manifest.FromV3(m)
		for i := range bad.ModelDefinitions {
			if bad.ModelDefinitions[i].ModelKey == "DiscountRule" {
				rels := append([]manifest.RelationDef(nil), bad.ModelDefinitions[i].Relations...)
				mutate(&rels[0])
				bad.ModelDefinitions[i].Relations = rels
			}
		}
		if err := bad.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "relations[0]") {
			t.Errorf("%s: legacy Validate = %v, want a relations[0] error", name, err)
		}
	}
}

// The strict schema carries min's lower bound (>= 0).
func TestRelationCreateInlineSchemaRejectsNegativeMin(t *testing.T) {
	bad := strings.Replace(inlineManifestJSON, `"min": 1`, `"min": -1`, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted min -1")
	}
}
