package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// visibleWhenManifestJSON declares a discount-rule model whose scope field
// (rule_scope) drives which picker is shown: product_id when scope=product,
// category_id when scope in {category}, customer_id when scope=customer. "all"
// shows none of the three. This exercises both the `equals` and `in` forms of
// the conditional-visibility predicate.
const visibleWhenManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "customers", "name": "Customers", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "DiscountRule",
      "table": "discount_rules",
      "label": "Discount Rules",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "name", "type": "text", "not_null": true },
        { "name": "rule_scope", "type": "text", "not_null": true,
          "options": [
            { "value": "all", "label": "Todos" },
            { "value": "product", "label": "Producto" },
            { "value": "category", "label": "Categoría" },
            { "value": "customer", "label": "Cliente" }
          ]
        },
        { "name": "product_id", "type": "uuid", "ref": "Product",
          "visible_when": { "field": "rule_scope", "equals": "product" } },
        { "name": "category_id", "type": "uuid", "ref": "Category",
          "visible_when": { "field": "rule_scope", "in": ["category"] } },
        { "name": "customer_id", "type": "uuid", "ref": "Customer",
          "visible_when": { "field": "rule_scope", "equals": "customer" } }
      ]
    }
  ]
}`

// TestColumnVisibleWhenParseAndProject walks visible_when through the whole
// kernel-side chain: v3.Parse (strict jsonschema + typed decode) → FromV3
// (legacy carrier) → legacy Validate → DeriveFormFields (served metadata). The
// predicate must survive both validation planes and land on the served form
// field so the SDK can show/hide it against the live form values.
func TestColumnVisibleWhenParseAndProject(t *testing.T) {
	// (a) strict schema + typed decode accepts and carries visible_when.
	m, err := v3.Parse([]byte(visibleWhenManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with visible_when: %v", err)
	}
	cols := map[string]v3.Column{}
	for _, c := range m.Models[0].Columns {
		cols[c.Name] = c
	}
	if vw := cols["product_id"].VisibleWhen; vw == nil || vw.Field != "rule_scope" || vw.Equals != "product" {
		t.Errorf("v3 product_id.VisibleWhen = %+v, want {field:rule_scope equals:product}", vw)
	}
	if vw := cols["category_id"].VisibleWhen; vw == nil || vw.Field != "rule_scope" || len(vw.In) != 1 || vw.In[0] != "category" {
		t.Errorf("v3 category_id.VisibleWhen = %+v, want {field:rule_scope in:[category]}", vw)
	}
	if cols["rule_scope"].VisibleWhen != nil {
		t.Errorf("v3 rule_scope.VisibleWhen = %+v, want nil (always visible)", cols["rule_scope"].VisibleWhen)
	}

	// (b) FromV3 carries it onto the legacy ColumnDef; (c) legacy Validate agrees.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "DiscountRule" {
			def = d
		}
	}
	legacy := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		legacy[c.Name] = c
	}
	if vw := legacy["customer_id"].VisibleWhen; vw == nil || vw.Field != "rule_scope" || vw.Equals != "customer" {
		t.Errorf("legacy customer_id.VisibleWhen = %+v, want {field:rule_scope equals:customer}", vw)
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// (d) DeriveFormFields projects visible_when onto the served form field.
	fields := map[string]*modelVisibleWhen{}
	for _, f := range dynamic.DeriveFormFields(def) {
		if f.VisibleWhen != nil {
			fields[f.Key] = &modelVisibleWhen{Field: f.VisibleWhen.Field, Equals: f.VisibleWhen.Equals, In: f.VisibleWhen.In}
		} else {
			fields[f.Key] = nil
		}
	}
	if vw := fields["product_id"]; vw == nil || vw.Field != "rule_scope" || vw.Equals != "product" {
		t.Errorf("served form product_id.VisibleWhen = %+v, want {field:rule_scope equals:product}", vw)
	}
	if vw := fields["category_id"]; vw == nil || len(vw.In) != 1 || vw.In[0] != "category" {
		t.Errorf("served form category_id.VisibleWhen = %+v, want in:[category]", vw)
	}
	if fields["name"] != nil {
		t.Errorf("served form name.VisibleWhen = %+v, want nil (always visible)", fields["name"])
	}
}

type modelVisibleWhen struct {
	Field  string
	Equals string
	In     []string
}
