package dyntest

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// documentEditorManifestJSON declares a payment model whose document type uses
// the DocumentEditor block (layout/party/sources/preview/submit_action, lines
// kind/discount_mode/open_documents) plus an action field seeded with
// default_from_record. Every key must reach the served metadata verbatim.
const documentEditorManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Customer",
      "table": "billing_customers",
      "label": "Customers",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "name", "type": "text" },
        { "name": "credit_limit", "type": "numeric" }
      ]
    },
    {
      "key": "Invoice",
      "table": "billing_invoices",
      "label": "Invoices",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "customer_id", "type": "uuid", "ref": "Customer" },
        { "name": "number", "type": "text" },
        { "name": "status", "type": "text" },
        { "name": "amount_due", "type": "numeric" }
      ]
    },
    {
      "key": "Payment",
      "table": "billing_payments",
      "label": "Payments",
      "document_forms": {
        "type_field": "type",
        "types": [
          {
            "key": "payment",
            "label": "Cobro",
            "fields": [
              { "key": "customer_id", "label": "Cliente", "type": "text", "required": true }
            ],
            "layout": "editor",
            "submit_action": "collect_payment_create",
            "party": { "field": "customer_id", "model": "Customer", "endpoint": "/data/customers", "summary": ["name"], "credit": { "limit": "credit_limit" } },
            "sources": [
              { "key": "invoice", "label": "Factura", "model": "Invoice", "lines": "items",
                "map": { "amount": "amount_due" }, "header": { "customer_id": "customer_id" },
                "link_field": "invoice_id", "option_filter": { "field": "status", "equals": "open" }, "requires_addon": "fiscal" }
            ],
            "preview": { "action": "preview_receipt", "requires_addon": "fiscal", "label": "Vista previa" },
            "lines": {
              "kind": "allocation",
              "discount_mode": "amount",
              "open_documents": {
                "model": "Invoice", "party_field": "customer_id", "balance_field": "amount_due", "number_field": "number",
                "line_document_field": "invoice_id", "line_amount_field": "amount",
                "option_filter": [ { "field": "status", "not_in": ["cancelled"] } ]
              }
            }
          }
        ]
      },
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "type", "type": "text" },
        { "name": "customer_id", "type": "uuid", "ref": "Customer" }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "register_payment",
        "label": "Registrar pago",
        "target_model": "Invoice",
        "handler": { "type": "wasm", "function": "register_payment" },
        "fields": [
          { "key": "invoice_id", "label": "Factura", "type": "text", "default_from_record": "id" },
          { "key": "customer_id", "label": "Cliente", "type": "text", "default_from_record": ["customer_id", "id"] }
        ]
      }
    ]
  }
}`

func TestDocumentEditorFieldsReachServedMetadata(t *testing.T) {
	m, err := v3.Parse([]byte(documentEditorManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected document editor manifest: %v", err)
	}
	host := manifest.FromV3(m)
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Payment" {
			def = d
		}
	}
	df := dynamic.DeriveDocumentForms(def)
	if df == nil || len(df.Types) != 1 {
		t.Fatalf("DeriveDocumentForms = %+v", df)
	}
	raw, err := json.Marshal(modelbase.TableMetadata{Title: "Payments", DocumentForms: df})
	if err != nil {
		t.Fatal(err)
	}
	var served map[string]any
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatal(err)
	}
	ty := served["document_forms"].(map[string]any)["types"].([]any)[0].(map[string]any)
	if ty["layout"] != "editor" || ty["submit_action"] != "collect_payment_create" {
		t.Errorf("layout/submit_action lost: %s", raw)
	}
	party, _ := ty["party"].(map[string]any)
	if party == nil || party["field"] != "customer_id" || party["model"] != "Customer" || party["endpoint"] != "/data/customers" ||
		party["summary"].([]any)[0] != "name" || party["credit"].(map[string]any)["limit"] != "credit_limit" {
		t.Errorf("party lost: %v", ty["party"])
	}
	srcs, _ := ty["sources"].([]any)
	if len(srcs) != 1 {
		t.Fatalf("sources lost: %v", ty["sources"])
	}
	src := srcs[0].(map[string]any)
	if src["key"] != "invoice" || src["label"] != "Factura" || src["model"] != "Invoice" || src["lines"] != "items" ||
		src["map"].(map[string]any)["amount"] != "amount_due" || src["header"].(map[string]any)["customer_id"] != "customer_id" ||
		src["link_field"] != "invoice_id" || src["requires_addon"] != "fiscal" {
		t.Errorf("source = %v", src)
	}
	if of, _ := src["option_filter"].([]any); len(of) != 1 || of[0].(map[string]any)["equals"] != "open" {
		t.Errorf("source option_filter = %v", src["option_filter"])
	}
	prev, _ := ty["preview"].(map[string]any)
	if prev == nil || prev["action"] != "preview_receipt" || prev["requires_addon"] != "fiscal" || prev["label"] != "Vista previa" {
		t.Errorf("preview lost: %v", ty["preview"])
	}
	lines, ok := ty["lines"].(map[string]any)
	if !ok {
		t.Fatalf("lines with only kind/discount_mode/open_documents served as %v (collapsed to true?)", ty["lines"])
	}
	if lines["kind"] != "allocation" || lines["discount_mode"] != "amount" {
		t.Errorf("lines = %v", lines)
	}
	od, _ := lines["open_documents"].(map[string]any)
	if od == nil || od["model"] != "Invoice" || od["party_field"] != "customer_id" || od["balance_field"] != "amount_due" ||
		od["number_field"] != "number" || od["line_document_field"] != "invoice_id" || od["line_amount_field"] != "amount" {
		t.Errorf("open_documents = %v", lines["open_documents"])
	}
	if of, _ := od["option_filter"].([]any); len(of) != 1 || of[0].(map[string]any)["field"] != "status" {
		t.Errorf("open_documents option_filter = %v", od["option_filter"])
	}

	// --- action field default_from_record -------------------------------
	var act *manifest.ActionDef
	for _, acts := range host.Actions {
		for i := range acts {
			if acts[i].Key == "register_payment" {
				act = &acts[i]
			}
		}
	}
	if act == nil || len(act.Fields) != 2 {
		t.Fatalf("register_payment not projected: %+v", act)
	}
	araw, _ := json.Marshal(act)
	var served2 modelbase.ActionDef
	if err := json.Unmarshal(araw, &served2); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(served2)
	var back map[string]any
	_ = json.Unmarshal(out, &back)
	fields := back["fields"].([]any)
	if fields[0].(map[string]any)["default_from_record"] != "id" {
		t.Errorf("default_from_record (string) lost: %s", out)
	}
	if l, _ := fields[1].(map[string]any)["default_from_record"].([]any); len(l) != 2 || l[0] != "customer_id" {
		t.Errorf("default_from_record (list) lost: %s", out)
	}
}
