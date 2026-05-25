package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// newFieldsManifestJSON exercises the additive v3 metadata that the strict
// schema previously rejected: Setting.description, Setting.type "number", and
// a column-level comment. All three must now Parse cleanly.
const newFieldsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "taxes", "name": "Taxes", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Rate",
      "table": "rates",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true, "default": "gen_random_uuid()" },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "percent", "type": "numeric", "not_null": true, "comment": "Tax rate as a percentage, e.g. 16.0" }
      ]
    }
  ],
  "settings": [
    {
      "key": "default_rate",
      "type": "number",
      "label": "Default tax rate",
      "description": "Applied when a product has no explicit rate.",
      "default": 16.0
    }
  ]
}`

func TestParse_SettingDescription_NumberType_ColumnComment(t *testing.T) {
	m, err := v3.Parse([]byte(newFieldsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with description/number/comment: %v", err)
	}

	if len(m.Settings) != 1 {
		t.Fatalf("expected 1 setting, got %d", len(m.Settings))
	}
	s := m.Settings[0]
	if s.Type != "number" {
		t.Errorf("setting type = %q, want number", s.Type)
	}
	if s.Description != "Applied when a product has no explicit rate." {
		t.Errorf("setting description = %q", s.Description)
	}

	if len(m.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(m.Models))
	}
	var got string
	for _, c := range m.Models[0].Columns {
		if c.Name == "percent" {
			got = c.Comment
		}
	}
	if got != "Tax rate as a percentage, e.g. 16.0" {
		t.Errorf("percent column comment = %q", got)
	}

	// FromV3 must still succeed; the new metadata is intentionally not mapped
	// onto the legacy structs (no slot for it), so we just assert no panic and
	// that the type carries across on the setting.
	out := manifest.FromV3(m)
	if len(out.Settings) != 1 || out.Settings[0].Type != "number" {
		t.Errorf("FromV3 settings = %+v, want one setting of type number", out.Settings)
	}
}
