package dyntest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// documentFormsManifestJSON declares a fiscal-document model with a guided
// create flow (two types: an invoice with a lines step, a payment receipt that
// picks a non-cancelled invoice) plus option_filter / extra_columns on a model
// column and on an action field. It is the contract the SDK (runtime-react
// 47.x, DocumentFormDialog + option-filter) consumes.
const documentFormsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "fiscal", "name": "Fiscal", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Invoice",
      "table": "fiscal_invoices",
      "label": "Invoices",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "folio", "type": "text" },
        { "name": "status", "type": "text" }
      ]
    },
    {
      "key": "FiscalDocument",
      "table": "fiscal_documents",
      "label": "Fiscal documents",
      "document_forms": {
        "type_field": "type",
        "lines_field": "lines",
        "types": [
          {
            "key": "invoice",
            "label": "Factura",
            "description": "Ingreso (I)",
            "icon": "receipt",
            "value": "I",
            "fields": [
              { "key": "customer_name", "label": "Cliente", "type": "text", "required": true },
              { "key": "payment_method", "label": "Método de pago", "type": "text" }
            ],
            "defaults": { "currency": "MXN" },
            "lines": { "columns": ["tax", "discount"], "price_source": "sale", "required": true, "title": "Partidas" },
            "submit_label": "Emitir"
          },
          {
            "key": "payment_receipt",
            "label": "REP",
            "value": "P",
            "fields": [
              {
                "key": "invoice_id", "label": "Factura pagada", "type": "dynamic_select", "required": true,
                "options": { "source": "Invoice", "value": "id", "label": "folio", "extra_columns": ["status"] },
                "option_filter": { "field": "status", "not_in": ["cancelada"] }
              }
            ],
            "lines": true
          },
          {
            "key": "global",
            "label": "Global",
            "fields": [ { "key": "period", "label": "Periodo", "type": "text" } ],
            "lines": false
          }
        ]
      },
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "type", "type": "text" },
        {
          "name": "invoice_id", "type": "uuid", "ref": "Invoice",
          "options": { "source": "Invoice", "value": "id", "label": "folio", "extra_columns": ["status", "created_at"] },
          "option_filter": [
            { "field": "status", "not_in": ["cancelada", "borrador"] },
            { "field": "folio", "not_equals": "X" }
          ]
        }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "apply_payment",
        "label": "Aplicar pago",
        "target_model": "FiscalDocument",
        "handler": { "type": "wasm", "function": "apply_payment" },
        "fields": [
          {
            "key": "invoice_id", "label": "Factura", "type": "dynamic_select",
            "options": { "source": "Invoice", "value": "id", "label": "folio", "extra_columns": ["status"] },
            "option_filter": { "field": "status", "in": ["vigente", "parcial"] }
          }
        ]
      }
    ]
  }
}`

func TestDocumentFormsAndOptionFilterChain(t *testing.T) {
	m, err := v3.Parse([]byte(documentFormsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected document_forms manifest: %v", err)
	}
	var fd v3.Model
	for _, mod := range m.Models {
		if mod.Key == "FiscalDocument" {
			fd = mod
		}
	}
	if fd.DocumentForms == nil || len(fd.DocumentForms.Types) != 3 {
		t.Fatalf("v3 DocumentForms = %+v, want 3 types", fd.DocumentForms)
	}
	if !fd.DocumentForms.Types[1].Lines.Enabled() || fd.DocumentForms.Types[2].Lines.Enabled() {
		t.Errorf("lines true/false not decoded: %+v / %+v", fd.DocumentForms.Types[1].Lines, fd.DocumentForms.Types[2].Lines)
	}

	host := manifest.FromV3(m)
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "FiscalDocument" {
			def = d
		}
	}

	// --- served table metadata ------------------------------------------
	df := dynamic.DeriveDocumentForms(def)
	if df == nil {
		t.Fatal("DeriveDocumentForms returned nil")
	}
	table := modelbase.TableMetadata{Title: "Fiscal documents", Columns: dynamic.DeriveTableColumns(def), DocumentForms: df}
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	var served map[string]any
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatal(err)
	}
	forms := served["document_forms"].(map[string]any)
	if forms["type_field"] != "type" || forms["lines_field"] != "lines" {
		t.Errorf("document_forms head = %v", forms)
	}
	types := forms["types"].([]any)
	inv := types[0].(map[string]any)
	if inv["key"] != "invoice" || inv["value"] != "I" || inv["label"] != "Factura" || inv["submit_label"] != "Emitir" || inv["icon"] != "receipt" {
		t.Errorf("invoice type = %v", inv)
	}
	if inv["defaults"].(map[string]any)["currency"] != "MXN" {
		t.Errorf("defaults lost: %v", inv["defaults"])
	}
	lines := inv["lines"].(map[string]any)
	if lines["price_source"] != "sale" || lines["title"] != "Partidas" || lines["required"] != true || len(lines["columns"].([]any)) != 2 {
		t.Errorf("invoice lines = %v", lines)
	}
	ifields := inv["fields"].([]any)
	if f0 := ifields[0].(map[string]any); f0["key"] != "customer_name" || f0["required"] != true || f0["type"] != "text" {
		t.Errorf("invoice field[0] = %v", f0)
	}
	rep := types[1].(map[string]any)
	if rep["lines"] != true {
		t.Errorf("option-less lines must serve as true, got %v", rep["lines"])
	}
	pf := rep["fields"].([]any)[0].(map[string]any)
	of := pf["option_filter"].([]any)
	if len(of) != 1 || of[0].(map[string]any)["field"] != "status" || of[0].(map[string]any)["not_in"].([]any)[0] != "cancelada" {
		t.Errorf("type field option_filter = %v", pf["option_filter"])
	}
	oc := pf["optionsConfig"].(map[string]any)
	if oc["extra_columns"].([]any)[0] != "status" {
		t.Errorf("type field optionsConfig = %v", oc)
	}
	glob := types[2].(map[string]any)
	if _, has := glob["lines"]; has {
		t.Errorf("lines:false must not be served, got %v", glob["lines"])
	}

	// --- column option_filter + extra_columns ---------------------------
	var col modelbase.ColumnDef
	for _, c := range table.Columns {
		if c.Key == "invoice_id" {
			col = c
		}
	}
	if len(col.OptionFilter) != 2 || col.OptionFilter[0].Field != "status" || len(col.OptionFilter[0].NotIn) != 2 || col.OptionFilter[1].NotEquals != "X" {
		t.Errorf("served column option_filter = %+v", col.OptionFilter)
	}
	if col.OptionsConfig == nil || len(col.OptionsConfig.ExtraColumns) != 2 || col.OptionsConfig.ExtraColumns[0] != "status" {
		t.Errorf("served column optionsConfig = %+v", col.OptionsConfig)
	}
	// The form field projection carries the same.
	for _, f := range dynamic.DeriveFormFields(def) {
		if f.Key == "invoice_id" && (len(f.OptionFilter) != 2 || f.OptionsConfig == nil || len(f.OptionsConfig.ExtraColumns) != 2) {
			t.Errorf("served form field = %+v", f)
		}
	}

	// --- action field ------------------------------------------------------
	var act *manifest.ActionDef
	for _, acts := range host.Actions {
		for i := range acts {
			if acts[i].Key == "apply_payment" {
				act = &acts[i]
			}
		}
	}
	if act == nil || len(act.Fields) != 1 {
		t.Fatalf("apply_payment action not projected: %+v", act)
	}
	araw, _ := json.Marshal(act.Fields[0])
	var af modelbase.FieldDef
	if err := json.Unmarshal(araw, &af); err != nil {
		t.Fatal(err)
	}
	if len(af.OptionFilter) != 1 || len(af.OptionFilter[0].In) != 2 || af.OptionsConfig == nil || af.OptionsConfig.ExtraColumns[0] != "status" {
		t.Errorf("served action field = %+v", af)
	}
}

// TestDocumentFormsAbsentIsInvisible: a model without document_forms serves a
// payload with no document_forms key and columns without option_filter.
func TestDocumentFormsAbsentIsInvisible(t *testing.T) {
	m, err := v3.Parse([]byte(formLayoutSectionsManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	host := manifest.FromV3(m)
	def := host.ModelDefinitions[0]
	if dynamic.DeriveDocumentForms(def) != nil {
		t.Fatal("DeriveDocumentForms must be nil without document_forms")
	}
	raw, _ := json.Marshal(modelbase.TableMetadata{Title: "x", Columns: dynamic.DeriveTableColumns(def)})
	if strings.Contains(string(raw), "document_forms") || strings.Contains(string(raw), "option_filter") || strings.Contains(string(raw), "extra_columns") {
		t.Errorf("payload leaked new keys: %s", raw)
	}
}

func TestDocumentFormsValidationErrors(t *testing.T) {
	mutations := []struct {
		name     string
		from, to string
		wantMsg  string
	}{
		{"type_field not a column", `"type_field": "type"`, `"type_field": "nope"`, "type_field"},
		{"duplicate type key", `"key": "payment_receipt"`, `"key": "invoice"`, "duplicated"},
		{"bad price_source", `"price_source": "sale"`, `"price_source": "wholesale"`, "price_source"},
		{"filter without operator", `"option_filter": { "field": "status", "in": ["vigente", "parcial"] }`, `"option_filter": { "field": "status" }`, "option_filter"},
		{"extra column reserved", `"extra_columns": ["status"] },
                "option_filter": { "field": "status", "not_in"`, `"extra_columns": ["label"] },
                "option_filter": { "field": "status", "not_in"`, "collides"},
		{"extra column not on source", `"extra_columns": ["status", "created_at"]`, `"extra_columns": ["status", "ghost"]`, "ghost"},
	}
	for _, mu := range mutations {
		t.Run(mu.name, func(t *testing.T) {
			if !strings.Contains(documentFormsManifestJSON, mu.from) {
				t.Skipf("anchor not found: %q", mu.from)
			}
			raw := strings.Replace(documentFormsManifestJSON, mu.from, mu.to, 1)
			_, err := v3.Parse([]byte(raw))
			if err == nil {
				t.Fatalf("v3.Parse accepted an invalid document_forms manifest (%s)", mu.name)
			}
			if mu.wantMsg != "" && !strings.Contains(err.Error(), mu.wantMsg) {
				t.Errorf("error %q does not mention %q", err, mu.wantMsg)
			}
		})
	}
}
