package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// formLayoutSectionsManifestJSON declares an Invoice model whose create/edit
// form is grouped into collapsible SECTIONS (single scroll). Columns bind to a
// section via `section`; the "notes" column omits it (implicit General block).
const formLayoutSectionsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Invoice",
      "table": "invoices",
      "label": "Invoices",
      "form_layout": {
        "mode": "sections",
        "sections": [
          { "key": "identity", "title": "Identidad", "description": "Datos base" },
          { "key": "totals", "title": "Totales", "collapsed": true }
        ]
      },
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "number", "type": "text", "not_null": true, "section": "identity" },
        { "name": "customer_id", "type": "uuid", "ref": "Customer", "section": "identity" },
        { "name": "total", "type": "numeric", "section": "totals" },
        { "name": "notes", "type": "text" }
      ]
    }
  ]
}`

// formLayoutStepsManifestJSON declares the same shape but as a STEP wizard.
const formLayoutStepsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Invoice",
      "table": "invoices",
      "label": "Invoices",
      "form_layout": {
        "mode": "steps",
        "sections": [
          { "key": "identity", "title": "Identidad" },
          { "key": "totals", "title": "Totales" }
        ]
      },
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "number", "type": "text", "not_null": true, "section": "identity" },
        { "name": "total", "type": "numeric", "section": "totals" }
      ]
    }
  ]
}`

// TestFormLayoutSectionsParseAndProject walks form_layout (mode "sections") +
// column.section through the whole kernel-side chain: v3.Parse (strict
// jsonschema + typed decode) → FromV3 (legacy carrier) → legacy Validate →
// DeriveFormFields / DeriveFormLayout (served metadata). The grouping must
// survive both validation planes and land on the served form metadata.
func TestFormLayoutSectionsParseAndProject(t *testing.T) {
	// (a) strict schema + typed decode accepts and carries form_layout/section.
	m, err := v3.Parse([]byte(formLayoutSectionsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with form_layout: %v", err)
	}
	fl := m.Models[0].FormLayout
	if fl == nil || fl.Mode != "sections" || len(fl.Sections) != 2 {
		t.Fatalf("v3 FormLayout = %+v, want mode=sections with 2 sections", fl)
	}
	if fl.Sections[0].Key != "identity" || fl.Sections[0].Title != "Identidad" || fl.Sections[0].Description != "Datos base" {
		t.Errorf("v3 section[0] = %+v, want {identity Identidad 'Datos base'}", fl.Sections[0])
	}
	if !fl.Sections[1].Collapsed {
		t.Errorf("v3 section[1].Collapsed = false, want true")
	}
	v3cols := map[string]v3.Column{}
	for _, c := range m.Models[0].Columns {
		v3cols[c.Name] = c
	}
	if v3cols["number"].Section != "identity" {
		t.Errorf("v3 number.Section = %q, want identity", v3cols["number"].Section)
	}
	if v3cols["notes"].Section != "" {
		t.Errorf("v3 notes.Section = %q, want empty (implicit General)", v3cols["notes"].Section)
	}

	// (b) FromV3 carries form_layout + section onto the legacy defs; (c) Validate.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Invoice" {
			def = d
		}
	}
	if def.FormLayout == nil || def.FormLayout.Mode != "sections" || len(def.FormLayout.Sections) != 2 {
		t.Fatalf("legacy FormLayout = %+v, want mode=sections with 2 sections", def.FormLayout)
	}
	if def.FormLayout.Sections[1].Key != "totals" || !def.FormLayout.Sections[1].Collapsed {
		t.Errorf("legacy section[1] = %+v, want {totals collapsed}", def.FormLayout.Sections[1])
	}
	legacy := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		legacy[c.Name] = c
	}
	if legacy["customer_id"].Section != "identity" {
		t.Errorf("legacy customer_id.Section = %q, want identity", legacy["customer_id"].Section)
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// (d) DeriveFormLayout projects the grouping; DeriveFormFields carries section.
	served := dynamic.DeriveFormLayout(def)
	if served == nil || served.Mode != "sections" || len(served.Sections) != 2 {
		t.Fatalf("served FormLayout = %+v, want mode=sections with 2 sections", served)
	}
	if served.Sections[0].Title != "Identidad" {
		t.Errorf("served section[0].Title = %q, want Identidad", served.Sections[0].Title)
	}
	fields := map[string]string{}
	seen := map[string]bool{}
	for _, f := range dynamic.DeriveFormFields(def) {
		fields[f.Key] = f.Section
		seen[f.Key] = true
	}
	if fields["number"] != "identity" {
		t.Errorf("served form number.Section = %q, want identity", fields["number"])
	}
	if fields["total"] != "totals" {
		t.Errorf("served form total.Section = %q, want totals", fields["total"])
	}
	if seen["notes"] && fields["notes"] != "" {
		t.Errorf("served form notes.Section = %q, want empty (implicit General)", fields["notes"])
	}
}

// TestFormLayoutStepsParseAndProject verifies the SAME primitive in mode
// "steps" (wizard) round-trips through the chain onto the served FormLayout.
func TestFormLayoutStepsParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(formLayoutStepsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with form_layout steps: %v", err)
	}
	if fl := m.Models[0].FormLayout; fl == nil || fl.Mode != "steps" || len(fl.Sections) != 2 {
		t.Fatalf("v3 FormLayout = %+v, want mode=steps with 2 steps", m.Models[0].FormLayout)
	}

	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Invoice" {
			def = d
		}
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected steps manifest: %v", err)
	}
	served := dynamic.DeriveFormLayout(def)
	if served == nil || served.Mode != "steps" || len(served.Sections) != 2 {
		t.Fatalf("served FormLayout = %+v, want mode=steps with 2 steps", served)
	}
	if served.Sections[0].Key != "identity" || served.Sections[1].Key != "totals" {
		t.Errorf("served steps = %+v, want [identity totals]", served.Sections)
	}
}

// formLayoutSectionVisibleWhenManifestJSON declares a form_layout whose second
// SECTION carries a visible_when predicate: the whole "totals" block is hidden
// unless the sibling "kind" field equals "invoice" (any-of via `in`).
const formLayoutSectionVisibleWhenManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Invoice",
      "table": "invoices",
      "label": "Invoices",
      "form_layout": {
        "mode": "sections",
        "sections": [
          { "key": "identity", "title": "Identidad" },
          { "key": "totals", "title": "Totales", "visible_when": { "field": "kind", "in": ["invoice", "credit_note"] } }
        ]
      },
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "kind", "type": "text", "section": "identity" },
        { "name": "total", "type": "numeric", "section": "totals" }
      ]
    }
  ]
}`

// TestFormLayoutSectionVisibleWhenParseAndProject walks a section-level
// visible_when through v3.Parse → FromV3 → Validate → DeriveFormLayout, asserting
// the {field, in} predicate survives every plane and lands on the served
// FormSection.VisibleWhen with the same snake key a column's visible_when uses.
func TestFormLayoutSectionVisibleWhenParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(formLayoutSectionVisibleWhenManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with section visible_when: %v", err)
	}
	vw := m.Models[0].FormLayout.Sections[1].VisibleWhen
	if vw == nil || vw.Field != "kind" || len(vw.In) != 2 || vw.In[0] != "invoice" {
		t.Fatalf("v3 section[1].VisibleWhen = %+v, want {field:kind in:[invoice credit_note]}", vw)
	}

	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Invoice" {
			def = d
		}
	}
	if def.FormLayout == nil || def.FormLayout.Sections[1].VisibleWhen == nil {
		t.Fatalf("legacy section[1].VisibleWhen dropped: %+v", def.FormLayout)
	}
	if def.FormLayout.Sections[1].VisibleWhen.Field != "kind" {
		t.Errorf("legacy section[1].VisibleWhen.Field = %q, want kind", def.FormLayout.Sections[1].VisibleWhen.Field)
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected section visible_when manifest: %v", err)
	}

	served := dynamic.DeriveFormLayout(def)
	if served == nil || len(served.Sections) != 2 {
		t.Fatalf("served FormLayout = %+v, want 2 sections", served)
	}
	sv := served.Sections[1].VisibleWhen
	if sv == nil || sv.Field != "kind" || len(sv.In) != 2 || sv.In[1] != "credit_note" {
		t.Fatalf("served section[1].VisibleWhen = %+v, want {field:kind in:[invoice credit_note]}", sv)
	}
	// The identity section declares no predicate → always visible.
	if served.Sections[0].VisibleWhen != nil {
		t.Errorf("served section[0].VisibleWhen = %+v, want nil (always visible)", served.Sections[0].VisibleWhen)
	}
}

// TestFormLayoutRejectsUnknownMode confirms the strict jsonschema enum on
// form_layout.mode rejects an unsupported presentation mode.
func TestFormLayoutRejectsUnknownMode(t *testing.T) {
	bad := `{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": { "key": "billing", "name": "Billing", "version": "0.1.0" },
      "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
      "models": [
        {
          "key": "Invoice", "table": "invoices", "label": "Invoices",
          "form_layout": { "mode": "tabs", "sections": [ { "key": "identity" } ] },
          "columns": [ { "name": "id", "type": "uuid", "primary_key": true } ]
        }
      ]
    }`
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatalf("v3.Parse accepted form_layout.mode=tabs, want strict-schema rejection")
	}
}
