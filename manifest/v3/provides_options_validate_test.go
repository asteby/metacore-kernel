package v3

import (
	"strings"
	"testing"
)

// providesOptionsManifest wraps a provides_options block (plus the model it
// publishes) in the minimum valid addon manifest, so Parse exercises the JSON
// schema — additionalProperties:false at the top level would reject
// "provides_options" outright if the schema entry were missing — and then
// validateProvidesOptions.
func providesOptionsManifest(catalogs string) []byte {
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "pos", "name": "POS", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "models": [
        {
          "key": "POSPaymentMethod",
          "table": "pos_payment_methods",
          "columns": [
            {"name": "organization_id", "type": "uuid"},
            {"name": "code", "type": "text"},
            {"name": "name", "type": "text"},
            {"name": "is_active", "type": "boolean"},
            {"name": "sort_order", "type": "integer"},
            {"name": "is_cash", "type": "boolean"}
          ]
        }
      ],
      "provides_options": ` + catalogs + `
    }`)
}

func TestProvidesOptionsParses(t *testing.T) {
	raw := providesOptionsManifest(`[
      {
        "key": "payment_methods",
        "model": "POSPaymentMethod",
        "value": "code",
        "label": "name",
        "where": {"is_active": true},
        "order_by": "sort_order",
        "extras": ["is_cash"]
      }
    ]`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.ProvidesOptions) != 1 {
		t.Fatalf("want 1 catalog, got %d", len(m.ProvidesOptions))
	}
	c := m.ProvidesOptions[0]
	if c.Key != "payment_methods" || c.Value != "code" || c.Label != "name" {
		t.Fatalf("catalog not parsed: %+v", c)
	}
	if c.OrderBy != "sort_order" || len(c.Extras) != 1 || c.Extras[0] != "is_cash" {
		t.Fatalf("order_by/extras not parsed: %+v", c)
	}
	if c.Where["is_active"] != true {
		t.Fatalf("where not parsed: %+v", c.Where)
	}
}

// The whole point of validating here is that a bad column would otherwise
// surface as a SQL error while serving metadata for a request that has
// nothing to do with the producer addon.
func TestProvidesOptionsRejectsUnknownColumnsAndModel(t *testing.T) {
	cases := map[string]struct{ block, want string }{
		"unknown model": {
			`[{"key": "pm", "model": "Nope", "value": "code", "label": "name"}]`,
			"is not a model of this addon",
		},
		"unknown value column": {
			`[{"key": "pm", "model": "POSPaymentMethod", "value": "nope", "label": "name"}]`,
			`.value "nope" is not a column`,
		},
		"unknown label column": {
			`[{"key": "pm", "model": "POSPaymentMethod", "value": "code", "label": "nope"}]`,
			`.label "nope" is not a column`,
		},
		"unknown order_by": {
			`[{"key": "pm", "model": "POSPaymentMethod", "value": "code", "label": "name", "order_by": "nope"}]`,
			`.order_by "nope" is not a column`,
		},
		"unknown extra": {
			`[{"key": "pm", "model": "POSPaymentMethod", "value": "code", "label": "name", "extras": ["nope"]}]`,
			`.extras[0] "nope" is not a column`,
		},
		"unknown where column": {
			`[{"key": "pm", "model": "POSPaymentMethod", "value": "code", "label": "name", "where": {"nope": true}}]`,
			`where names "nope"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(providesOptionsManifest(tc.block))
			if err == nil {
				t.Fatalf("want a validation error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestProvidesOptionsRejectsDuplicateKeyWithinManifest(t *testing.T) {
	raw := providesOptionsManifest(`[
      {"key": "pm", "model": "POSPaymentMethod", "value": "code", "label": "name"},
      {"key": "pm", "model": "POSPaymentMethod", "value": "name", "label": "code"}
    ]`)
	if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), "is duplicated") {
		t.Fatalf("want a duplicate-key error, got %v", err)
	}
}

// The schema's identifier grammar has to hold for the catalog key: it is the
// literal string a consumer writes in options_source.
func TestProvidesOptionsRejectsNonIdentifierKey(t *testing.T) {
	raw := providesOptionsManifest(`[
      {"key": "Payment Methods", "model": "POSPaymentMethod", "value": "code", "label": "name"}
    ]`)
	if _, err := Parse(raw); err == nil {
		t.Fatalf("want a schema error for a non-identifier key")
	}
}
