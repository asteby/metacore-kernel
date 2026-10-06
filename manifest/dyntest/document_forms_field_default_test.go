package dyntest

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Retest r4 Pitsline (06/10, FAC-00014): a document_forms field's `default`
// (manifest) never reached the served metadata — the carrier spells it
// `default`, modelbase.FieldDef `defaultValue`, and DeriveDocumentForms
// round-trips through JSON. The invoice date could not default to today.
const documentFormsFieldDefaultJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "sales", "name": "Sales", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Invoice",
      "table": "invoices",
      "label": "Invoices",
      "document_forms": {
        "types": [
          {
            "key": "invoice",
            "label": "Factura",
            "fields": [
              { "key": "customer_id", "label": "Cliente", "type": "text", "required": true },
              { "key": "invoice_date", "label": "Fecha", "type": "date", "required": true, "default": "$today" },
              { "key": "fiscal_data.metodo_pago", "label": "Método", "type": "select", "default": "PUE",
                "options": [ { "value": "PUE", "label": "PUE" }, { "value": "PPD", "label": "PPD" } ] }
            ]
          }
        ]
      },
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "customer_id", "type": "uuid" },
        { "name": "invoice_date", "type": "timestamptz" }
      ]
    }
  ]
}`

func TestDeriveDocumentForms_ServesFieldDefaults(t *testing.T) {
	m, err := v3.Parse([]byte(documentFormsFieldDefaultJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)
	df := dynamic.DeriveDocumentForms(host.ModelDefinitions[0])
	if df == nil || len(df.Types) != 1 {
		t.Fatalf("DeriveDocumentForms = %+v", df)
	}
	raw, _ := json.Marshal(df)
	var served struct {
		Types []struct {
			Fields []map[string]any `json:"fields"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatal(err)
	}
	f := served.Types[0].Fields
	if _, has := f[0]["defaultValue"]; has {
		t.Errorf("a field without default must not serve one: %v", f[0])
	}
	if f[1]["defaultValue"] != "$today" {
		t.Errorf("invoice_date defaultValue = %v, want $today (served %s)", f[1]["defaultValue"], raw)
	}
	if f[2]["defaultValue"] != "PUE" {
		t.Errorf("metodo_pago defaultValue = %v, want PUE", f[2]["defaultValue"])
	}
}
