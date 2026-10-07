package v3

import (
	"strings"
	"testing"
)

// optionDisplayManifest wraps an option_display (on Product) and an
// option_metrics block (aggregating Stock) in a minimal addon so Parse runs the
// JSON schema (additionalProperties:false) and the cross-field validation.
func optionDisplayManifest(display, metrics string) []byte {
	if display == "" {
		display = `{"title": "name"}`
	}
	if metrics == "" {
		metrics = "[]"
	}
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "inventory", "name": "Inventory", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "models": [
        {
          "key": "Product", "table": "products",
          "columns": [
            {"name": "organization_id", "type": "uuid"},
            {"name": "name", "type": "text"},
            {"name": "sku", "type": "text"},
            {"name": "unit_price", "type": "numeric"},
            {"name": "currency_code", "type": "text"},
            {"name": "min_stock", "type": "numeric"},
            {"name": "image", "type": "text"},
            {"name": "product_type", "type": "text"}
          ],
          "option_display": ` + display + `
        },
        {
          "key": "Stock", "table": "stock",
          "columns": [
            {"name": "organization_id", "type": "uuid"},
            {"name": "product_id", "type": "uuid"},
            {"name": "warehouse_id", "type": "uuid"},
            {"name": "variant_id", "type": "uuid"},
            {"name": "available", "type": "numeric"}
          ]
        },
        {
          "key": "Warehouse", "table": "warehouses",
          "columns": [
            {"name": "organization_id", "type": "uuid"},
            {"name": "branch_id", "type": "uuid"}
          ]
        }
      ],
      "option_metrics": ` + metrics + `
    }`)
}

const validDisplay = `{
  "title": "name",
  "subtitle": ["sku", "Tipo {product_type}"],
  "image": "image",
  "trailing": [
    {"key": "price", "label": "Precio", "field": "unit_price", "format": "money", "currency_field": "currency_code"},
    {"key": "stock", "label": "Disp.", "metric": "stock_available", "format": "number",
     "when": {"field": "product_type", "op": "neq", "value": "service"}, "tones": [
      {"when": {"op": "lte", "value": 0}, "tone": "danger", "text": "Agotado", "dim": true},
      {"when": {"op": "lte", "ref": "min_stock"}, "tone": "warning"},
      {"when": {"op": "gt", "value": 0}, "tone": "success"}
    ]}
  ],
  "badges": [
    {"field": "product_type", "when": {"op": "eq", "value": "service"}, "text": "Servicio", "tone": "info"},
    {"field": "product_type", "values": {"bundle": {"text": "Paquete", "tone": "neutral"}}}
  ]
}`

const validMetrics = `[{
  "key": "stock_available", "target": "products.Product", "model": "Stock",
  "foreign_key": "product_id", "aggregate": "sum", "column": "available",
  "where": {"variant_id": null},
  "scope": [
    {"context": "warehouse_id", "column": "warehouse_id"},
    {"context": "branch_id", "column": "warehouse_id", "through": {"model": "Warehouse", "column": "branch_id"}}
  ]
}]`

func TestOptionDisplayParses(t *testing.T) {
	m, err := Parse(optionDisplayManifest(validDisplay, validMetrics))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := m.Models[0].OptionDisplay
	if d == nil || d.Title != "name" || len(d.Subtitle) != 2 || len(d.Trailing) != 2 || len(d.Badges) != 2 {
		t.Fatalf("display not parsed: %+v", d)
	}
	if tn := d.Trailing[1].Tones[0]; tn.Tone != "danger" || tn.Text != "Agotado" || !tn.Dim {
		t.Fatalf("tone not parsed: %+v", tn)
	}
	if len(m.OptionMetrics) != 1 || m.OptionMetrics[0].Scope[1].Through.Model != "Warehouse" {
		t.Fatalf("metrics not parsed: %+v", m.OptionMetrics)
	}
}

func TestOptionDisplayRejects(t *testing.T) {
	cases := map[string]struct{ display, metrics, want string }{
		"unknown title column":        {`{"title": "nope"}`, "", `title references "nope"`},
		"template without column":     {`{"subtitle": ["just text"]}`, "", "names no column"},
		"unknown placeholder":         {`{"subtitle": ["SKU {nope}"]}`, "", `references "nope"`},
		"unknown image":               {`{"image": "nope"}`, "", `.image "nope"`},
		"field and metric":            {`{"trailing": [{"key": "x", "field": "sku", "metric": "m"}]}`, "", "mutually exclusive"},
		"neither field nor metric":    {`{"trailing": [{"key": "x"}]}`, "", "one of field / metric"},
		"unknown trailing field":      {`{"trailing": [{"key": "x", "field": "nope"}]}`, "", `.field "nope"`},
		"duplicate key":               {`{"trailing": [{"key": "x", "field": "sku"}, {"key": "x", "field": "name"}]}`, "", "is duplicated"},
		"currency without money":      {`{"trailing": [{"key": "x", "field": "sku", "format": "number", "currency_field": "currency_code"}]}`, "", "only valid with format money"},
		"tone two operands":           {`{"trailing": [{"key": "x", "field": "unit_price", "tones": [{"when": {"op": "lt", "value": 1, "ref": "min_stock"}, "tone": "danger"}]}]}`, "", "exactly one of value"},
		"tone unknown ref":            {`{"trailing": [{"key": "x", "field": "unit_price", "tones": [{"when": {"op": "lt", "ref": "nope"}, "tone": "danger"}]}]}`, "", `.ref "nope"`},
		"tone with field":             {`{"trailing": [{"key": "x", "field": "unit_price", "tones": [{"when": {"field": "sku", "op": "empty"}, "tone": "danger"}]}]}`, "", "only valid on a badge"},
		"trailing when unknown field": {`{"trailing": [{"key": "x", "field": "sku", "when": {"field": "nope", "op": "empty"}}]}`, "", `.when.field "nope"`},
		"badge values without field":  {`{"badges": [{"text": "x", "values": {"a": {"text": "A"}}}]}`, "", "values requires field"},
		"metric unknown model":        {"", `[{"key": "s", "target": "products.Product", "model": "Nope", "foreign_key": "product_id", "aggregate": "sum", "column": "available"}]`, `.model "Nope"`},
		"metric unknown fk":           {"", `[{"key": "s", "target": "products.Product", "model": "Stock", "foreign_key": "nope", "aggregate": "sum", "column": "available"}]`, `.foreign_key "nope"`},
		"metric sum no column":        {"", `[{"key": "s", "target": "products.Product", "model": "Stock", "foreign_key": "product_id", "aggregate": "sum"}]`, ".column is empty"},
		"metric own target":           {"", `[{"key": "s", "target": "inventory.Nope", "model": "Stock", "foreign_key": "product_id", "aggregate": "count"}]`, "is not a model of this addon"},
		"metric where column":         {"", `[{"key": "s", "target": "products.Product", "model": "Stock", "foreign_key": "product_id", "aggregate": "count", "where": {"nope": 1}}]`, `where names "nope"`},
		"metric scope through":        {"", `[{"key": "s", "target": "products.Product", "model": "Stock", "foreign_key": "product_id", "aggregate": "count", "scope": [{"context": "branch_id", "column": "warehouse_id", "through": {"model": "Warehouse", "column": "nope"}}]}]`, `through.column "nope"`},
		"metric duplicate":            {"", `[{"key": "s", "target": "products.Product", "model": "Stock", "foreign_key": "product_id", "aggregate": "count"}, {"key": "s", "target": "products.Product", "model": "Stock", "foreign_key": "product_id", "aggregate": "count"}]`, "is duplicated"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(optionDisplayManifest(tc.display, tc.metrics))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// The schema's closed vocabularies reject what Validate would never see.
func TestOptionDisplaySchemaRejects(t *testing.T) {
	for name, tc := range map[string]struct{ display, metrics string }{
		"unknown format":     {`{"trailing": [{"key": "x", "field": "sku", "format": "currency"}]}`, ""},
		"unknown tone":       {`{"trailing": [{"key": "x", "field": "sku", "tones": [{"when": {"op": "empty"}, "tone": "red"}]}]}`, ""},
		"unknown op":         {`{"trailing": [{"key": "x", "field": "sku", "tones": [{"when": {"op": "between"}, "tone": "danger"}]}]}`, ""},
		"unknown property":   {`{"title": "name", "color": "red"}`, ""},
		"unknown aggregate":  {"", `[{"key": "s", "target": "products.Product", "model": "Stock", "foreign_key": "product_id", "aggregate": "median", "column": "available"}]`},
		"unqualified target": {"", `[{"key": "s", "target": "Product", "model": "Stock", "foreign_key": "product_id", "aggregate": "count"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(optionDisplayManifest(tc.display, tc.metrics)); err == nil {
				t.Fatal("want a schema error")
			}
		})
	}
}

func TestOptionTemplateColumns(t *testing.T) {
	if got := OptionTemplateColumns("sku"); len(got) != 1 || got[0] != "sku" {
		t.Fatalf("bare = %v", got)
	}
	if got := OptionTemplateColumns("{folio} · {status}"); strings.Join(got, ",") != "folio,status" {
		t.Fatalf("template = %v", got)
	}
}
