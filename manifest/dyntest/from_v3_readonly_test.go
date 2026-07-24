package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// readonlyManifestJSON declares a model with a SYSTEM-GENERATED column: `number`
// is populated by the addon after an outbound write (a value the user never
// types), so it is marked readonly.
const readonlyManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "integration_github", "name": "GitHub", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Issue",
      "table": "github_issues",
      "label": "Issues",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "title", "type": "text", "not_null": true },
        { "name": "number", "type": "bigint", "readonly": true }
      ]
    }
  ]
}`

// TestColumnReadonlyParseAndProject walks the readonly flag through the whole
// kernel-side chain: v3.Parse (strict jsonschema + typed decode) → FromV3
// (legacy carrier) → legacy Validate (the second, stricter plane) →
// DeriveFormFields (served metadata). Both validation planes must accept it and
// the flag must land on the served form field.
func TestColumnReadonlyParseAndProject(t *testing.T) {
	// (a) strict schema + typed decode accepts and carries readonly.
	m, err := v3.Parse([]byte(readonlyManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with column readonly: %v", err)
	}
	cols := map[string]v3.Column{}
	for _, c := range m.Models[0].Columns {
		cols[c.Name] = c
	}
	if !cols["number"].Readonly {
		t.Errorf("v3 number.Readonly = false, want true")
	}
	if cols["title"].Readonly {
		t.Errorf("v3 title.Readonly = true, want false")
	}

	// (b) FromV3 carries it onto the legacy ColumnDef; (c) legacy Validate agrees.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Issue" {
			def = d
		}
	}
	legacy := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		legacy[c.Name] = c
	}
	if !legacy["number"].Readonly {
		t.Errorf("legacy number.Readonly = false, want true")
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// (d) DeriveFormFields projects readonly onto the served form field.
	fields := map[string]bool{}
	for _, f := range dynamic.DeriveFormFields(def) {
		fields[f.Key] = f.Readonly
	}
	if !fields["number"] {
		t.Errorf("served form number.Readonly = false, want true")
	}
	if fields["title"] {
		t.Errorf("served form title.Readonly = true, want false")
	}
}
