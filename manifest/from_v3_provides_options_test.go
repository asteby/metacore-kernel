package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// The projection is the only layer that still sees models[], so it is where
// the catalog's model key becomes a physical table. The host reads Table and
// never re-derives it.
func TestFromV3ProjectsProvidesOptionsWithResolvedTable(t *testing.T) {
	raw := []byte(`{
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
            {"name": "is_cash", "type": "boolean"}
          ]
        }
      ],
      "provides_options": [
        {
          "key": "payment_methods",
          "model": "POSPaymentMethod",
          "value": "code",
          "label": "name",
          "where": {"is_active": true},
          "extras": ["is_cash"]
        }
      ]
    }`)
	v3m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	m := FromV3(v3m)
	if len(m.ProvidesOptions) != 1 {
		t.Fatalf("want 1 projected catalog, got %d", len(m.ProvidesOptions))
	}
	c := m.ProvidesOptions[0]
	if c.Table != "pos_payment_methods" {
		t.Fatalf("model key was not resolved to a table: %+v", c)
	}
	if c.Key != "payment_methods" || c.Value != "code" || c.Label != "name" {
		t.Fatalf("catalog not projected: %+v", c)
	}
	if c.Where["is_active"] != true || len(c.Extras) != 1 {
		t.Fatalf("where/extras not projected: %+v", c)
	}
}
