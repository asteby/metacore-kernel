package dyntest

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// optionsSourceManifestJSON declares a model whose column opts into DYNAMIC
// options via options_source: instead of hardcoding an options list, it names
// a host-registered provider ("registered_models") that materialises the
// localized value/label list at metadata-serve time.
const optionsSourceManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "automations", "name": "Automations", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Rule",
      "table": "addon_automation_rules",
      "label": "Rules",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "name", "type": "text" },
        { "name": "target_model", "type": "text", "options_source": "registered_models" },
        {
          "name": "status",
          "type": "text",
          "options": [ { "value": "on", "label": "On" }, { "value": "off", "label": "Off" } ]
        }
      ]
    }
  ]
}`

// TestColumnOptionsSourceParseAndProject walks options_source through the full
// kernel-side chain: v3.Parse (schema + typed decode) → FromV3 (legacy
// ColumnDef carrier) → DeriveTableColumns / DeriveFormFields (served
// modelbase metadata the host hands to the SDK). The host's provider registry
// then only has to read OptionsSource off the served defs and materialise
// Options — nothing else to wire.
func TestColumnOptionsSourceParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(optionsSourceManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with column options_source: %v", err)
	}

	// (a) the v3 typed shape carries options_source.
	cols3 := map[string]v3.Column{}
	for _, c := range m.Models[0].Columns {
		cols3[c.Name] = c
	}
	if got := cols3["target_model"].OptionsSource; got != "registered_models" {
		t.Errorf("v3 target_model.OptionsSource = %q, want registered_models", got)
	}
	if got := cols3["status"].OptionsSource; got != "" {
		t.Errorf("v3 status.OptionsSource = %q, want empty (static options column)", got)
	}

	// (b) FromV3 projects options_source onto the legacy ColumnDef carrier.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Rule" {
			def = d
		}
	}
	legacy := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		legacy[c.Name] = c
	}
	if got := legacy["target_model"].OptionsSource; got != "registered_models" {
		t.Errorf("legacy target_model.OptionsSource = %q, want registered_models", got)
	}

	// (c) the legacy (strict, hub/install-time) validation accepts the
	// converted manifest — the "double validation" planes agree.
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// (d) DeriveTableColumns projects options_source onto the served column
	// metadata so the host's provider registry sees it.
	served := map[string]struct {
		src        string
		useOptions bool
	}{}
	for _, c := range dynamic.DeriveTableColumns(def) {
		served[c.Key] = struct {
			src        string
			useOptions bool
		}{c.OptionsSource, c.UseOptions}
	}
	if got := served["target_model"].src; got != "registered_models" {
		t.Errorf("served target_model.OptionsSource = %q, want registered_models", got)
	}
	if served["target_model"].useOptions {
		t.Errorf("served target_model.UseOptions = true before host materialisation, want false")
	}

	// (e) DeriveFormFields projects options_source and renders the field as a
	// select (the host fills the choices; the input plane matches a static
	// choice list).
	fields := map[string]struct {
		src   string
		ftype string
	}{}
	for _, f := range dynamic.DeriveFormFields(def) {
		fields[f.Key] = struct {
			src   string
			ftype string
		}{f.OptionsSource, f.Type}
	}
	if got := fields["target_model"].src; got != "registered_models" {
		t.Errorf("form target_model.OptionsSource = %q, want registered_models", got)
	}
	if got := fields["target_model"].ftype; got != "select" {
		t.Errorf("form target_model.Type = %q, want select", got)
	}
}

// TestV3OptionsSourceBadFormatRejected proves the schema pattern gates the
// provider-key alphabet (lowercase snake only).
func TestV3OptionsSourceBadFormatRejected(t *testing.T) {
	bad := strings.Replace(optionsSourceManifestJSON, `"registered_models"`, `"Registered-Models"`, 1)
	if err := v3.Validate([]byte(bad)); err == nil {
		t.Fatal("v3.Validate accepted options_source with uppercase/dash, want pattern rejection")
	}
}

// TestLegacyValidateOptionsSourceFormat proves the legacy validator enforces
// the SAME alphabet as the v3 schema pattern — and accepts valid keys.
func TestLegacyValidateOptionsSourceFormat(t *testing.T) {
	mk := func(src string) manifest.Manifest {
		return manifest.Manifest{
			Key:     "automations",
			Name:    "Automations",
			Version: "1.0.0",
			Kernel:  ">=2.0.0",
			ModelDefinitions: []manifest.ModelDefinition{{
				TableName: "addon_automation_rules",
				ModelKey:  "rules",
				Columns: []manifest.ColumnDef{
					{Name: "target_model", Type: "string", OptionsSource: src},
				},
			}},
		}
	}
	good := mk("installed_addons")
	if err := good.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected valid options_source: %v", err)
	}
	bad := mk("Registered-Models")
	err := bad.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "options_source") {
		t.Fatalf("legacy Validate accepted bad options_source, got %v", err)
	}
}
