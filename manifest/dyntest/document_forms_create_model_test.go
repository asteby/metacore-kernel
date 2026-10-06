package dyntest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Retest r4 Pitsline (06/10): Documentos fiscales → Facturas lost «Crear»
// (the SDK hides it when the filtered type has no form). A type may delegate
// its create to another model's own flow: `create_model`.
func createModelManifest(typeJSON string) string {
	return `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "fiscal", "name": "Fiscal", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "FiscalDocument",
      "table": "fiscal_documents",
      "label": "Fiscal documents",
      "document_forms": {
        "type_field": "document_type",
        "types": [
          { "key": "payment_complement", "label": "REP", "value": "payment_complement",
            "fields": [ { "key": "amount", "label": "Monto", "type": "number" } ] },
          ` + typeJSON + `
        ]
      },
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "document_type", "type": "text" }
      ]
    }
  ]
}`
}

func TestDocumentForms_CreateModelIsServed(t *testing.T) {
	raw := createModelManifest(`{ "key": "invoice", "label": "Factura", "value": "invoice", "fields": [], "create_model": "customers.Invoice" }`)
	m, err := v3.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("v3.Parse rejected create_model: %v", err)
	}
	host := manifest.FromV3(m)
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate: %v", err)
	}
	df := dynamic.DeriveDocumentForms(host.ModelDefinitions[0])
	if df == nil || len(df.Types) != 2 {
		t.Fatalf("DeriveDocumentForms = %+v", df)
	}
	served, _ := json.Marshal(df)
	if !strings.Contains(string(served), `"create_model":"customers.Invoice"`) {
		t.Fatalf("served document_forms without create_model: %s", served)
	}
	if df.Types[0].CreateModel != "" {
		t.Fatalf("a regular type must not carry create_model: %+v", df.Types[0])
	}
}

func TestDocumentForms_CreateModelValidation(t *testing.T) {
	cases := map[string]string{
		"bad key":       `{ "key": "invoice", "label": "Factura", "fields": [], "create_model": "customers/Invoice" }`,
		"with fields":   `{ "key": "invoice", "label": "Factura", "fields": [ { "key": "x", "label": "X", "type": "text" } ], "create_model": "customers.Invoice" }`,
		"with lines":    `{ "key": "invoice", "label": "Factura", "fields": [], "lines": true, "create_model": "customers.Invoice" }`,
		"with endpoint": `{ "key": "invoice", "label": "Factura", "fields": [], "endpoint": "/data/invoices/me", "create_model": "customers.Invoice" }`,
		"with editor":   `{ "key": "invoice", "label": "Factura", "fields": [], "layout": "editor", "create_model": "customers.Invoice" }`,
	}
	for name, typ := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v3.Parse([]byte(createModelManifest(typ))); err == nil || !strings.Contains(err.Error(), "create_model") {
				t.Fatalf("want a create_model validation error, got %v", err)
			}
		})
	}
	for _, ok := range []string{"Invoice", "customers.Invoice", "invoices"} {
		typ := `{ "key": "invoice", "label": "Factura", "fields": [], "create_model": "` + ok + `" }`
		if _, err := v3.Parse([]byte(createModelManifest(typ))); err != nil {
			t.Errorf("create_model %q rejected: %v", ok, err)
		}
	}
}
