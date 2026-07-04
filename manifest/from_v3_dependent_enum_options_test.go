package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// dependentEnumManifestJSON exercises the STATIC dependent-enum-options
// contract: a model column `provider` and an action item_field carry the static
// array form of `options` where each option is gated by a `when` block against a
// sibling enum field (`type`). The provider select only offers qr/meta while
// type == "whatsapp"; the SDK hides it otherwise. depends_on names the governing
// sibling once at the field level; the options omit `when.field` and fall back
// to it.
const dependentEnumManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "link_inbox", "name": "Link Inbox", "version": "0.2.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
  "models": [
    {
      "key": "Device",
      "table": "addon_link_inbox_devices",
      "label": "Devices",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        {
          "name": "type",
          "type": "text",
          "options": [
            { "value": "whatsapp", "label": "WhatsApp" },
            { "value": "sms", "label": "SMS" }
          ]
        },
        {
          "name": "provider",
          "type": "text",
          "default": "qr",
          "depends_on": "type",
          "options": [
            { "value": "qr",   "label": "QR",   "color": "amber",
              "when": { "field": "type", "in": ["whatsapp"] } },
            { "value": "meta", "label": "Meta", "color": "blue",
              "when": { "in": ["whatsapp"] } }
          ]
        }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "connect_device",
        "label": "Connect",
        "target_model": "Device",
        "handler": { "type": "compiled", "function": "OnConnect" },
        "placement": "create",
        "fields": [
          {
            "key": "type",
            "label": "Type",
            "type": "select",
            "options": [
              { "value": "whatsapp", "label": "WhatsApp" },
              { "value": "sms", "label": "SMS" }
            ]
          },
          {
            "key": "provider",
            "label": "Provider",
            "type": "select",
            "depends_on": "type",
            "options": [
              { "value": "qr",   "label": "QR",
                "when": { "field": "type", "in": ["whatsapp"] } },
              { "value": "meta", "label": "Meta", "when": { "not_in": ["sms"] } }
            ]
          }
        ]
      }
    ]
  }
}`

func TestFromV3_DependentEnumOptionsRoundTrip(t *testing.T) {
	m, err := v3.Parse([]byte(dependentEnumManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected dependent-enum manifest: %v", err)
	}

	// (a) v3 typed shape: the provider column carries depends_on + static
	// options whose `when` blocks decoded onto FieldOption.When.
	var col v3.Column
	for _, c := range m.Models[0].Columns {
		if c.Name == "provider" {
			col = c
		}
	}
	if col.DependsOn != "type" {
		t.Errorf("v3 column depends_on = %q, want type", col.DependsOn)
	}
	if len(col.Options.Static) != 2 {
		t.Fatalf("v3 column static options len = %d, want 2", len(col.Options.Static))
	}
	if w := col.Options.Static[0].When; w == nil || w.Field != "type" || len(w.In) != 1 || w.In[0] != "whatsapp" {
		t.Errorf("v3 option[0].when = %+v", w)
	}
	if w := col.Options.Static[1].When; w == nil || w.Field != "" || len(w.In) != 1 {
		t.Errorf("v3 option[1].when = %+v (field should fall back to depends_on)", w)
	}

	// (b) FromV3 → legacy carrier column keeps depends_on + option.When.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Device" {
			def = d
		}
	}
	var legacyCol manifest.ColumnDef
	for _, c := range def.Columns {
		if c.Name == "provider" {
			legacyCol = c
		}
	}
	if legacyCol.DependsOn != "type" {
		t.Errorf("legacy column depends_on = %q, want type", legacyCol.DependsOn)
	}
	if len(legacyCol.Options) != 2 {
		t.Fatalf("legacy column options len = %d, want 2", len(legacyCol.Options))
	}
	if w := legacyCol.Options[0].When; w == nil || w.Field != "type" || w.In[0] != "whatsapp" {
		t.Errorf("legacy option[0].When = %+v", w)
	}

	// (c) DeriveFormFields → served modelbase FieldDef carries depends_on +
	// OptionDef.When on the static option list.
	var formProvider modelbase.FieldDef
	for _, f := range dynamic.DeriveFormFields(def) {
		if f.Key == "provider" {
			formProvider = f
		}
	}
	if formProvider.DependsOn != "type" {
		t.Errorf("served form provider.DependsOn = %q, want type", formProvider.DependsOn)
	}
	if len(formProvider.Options) != 2 {
		t.Fatalf("served form provider.Options len = %d, want 2", len(formProvider.Options))
	}
	if w := formProvider.Options[0].When; w == nil || w.Field != "type" || len(w.In) != 1 || w.In[0] != "whatsapp" {
		t.Errorf("served form provider.Options[0].When = %+v", w)
	}
	if w := formProvider.Options[1].When; w == nil || len(w.In) != 1 {
		t.Errorf("served form provider.Options[1].When = %+v", w)
	}

	// (d) served table-metadata column also carries When.
	cols := dynamic.DeriveTableColumns(def)
	var metaProvider modelbase.ColumnDef
	for _, c := range cols {
		if c.Key == "provider" {
			metaProvider = c
		}
	}
	if len(metaProvider.Options) != 2 || metaProvider.Options[0].When == nil {
		t.Errorf("served column provider.Options lost When: %+v", metaProvider.Options)
	}

	// (e) action field: depends_on + option.When threaded onto the served field.
	hostField := host.Actions["Device"][0].Fields[1]
	if hostField.Key != "provider" || hostField.DependsOn != "type" {
		t.Fatalf("legacy action field = %+v", hostField)
	}
	if len(hostField.Options) != 2 || hostField.Options[0].When == nil ||
		hostField.Options[0].When.Field != "type" {
		t.Errorf("legacy action field options lost When: %+v", hostField.Options)
	}
	if w := hostField.Options[1].When; w == nil || len(w.NotIn) != 1 || w.NotIn[0] != "sms" {
		t.Errorf("legacy action field option[1].When not_in lost: %+v", w)
	}

	// (f) strict legacy validator accepts the converted manifest.
	if err := host.Validate("3.0.0"); err != nil {
		t.Errorf("strict Validate rejected valid dependent-enum manifest: %v", err)
	}
}

// TestFromV3_DependentEnumOptionsRetrocompat proves an enum WITHOUT any `when`
// block is untouched: no When is populated anywhere along the derive chain.
func TestFromV3_DependentEnumOptionsRetrocompat(t *testing.T) {
	const plain = `{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": { "key": "plain", "name": "Plain", "version": "0.1.0" },
      "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
      "models": [
        {
          "key": "Thing", "table": "addon_plain_things", "label": "Things",
          "columns": [
            { "name": "id", "type": "uuid", "primary_key": true },
            { "name": "status", "type": "text",
              "options": [ { "value": "open", "label": "Open" }, { "value": "done", "label": "Done" } ] }
          ]
        }
      ]
    }`
	m, err := v3.Parse([]byte(plain))
	if err != nil {
		t.Fatalf("v3.Parse rejected plain enum manifest: %v", err)
	}
	host := manifest.FromV3(m)
	def := host.ModelDefinitions[0]
	for _, f := range dynamic.DeriveFormFields(def) {
		if f.Key != "status" {
			continue
		}
		for i, o := range f.Options {
			if o.When != nil {
				t.Errorf("plain enum option[%d] got a non-nil When: %+v", i, o.When)
			}
		}
	}
	if err := host.Validate("3.0.0"); err != nil {
		t.Errorf("strict Validate rejected plain enum manifest: %v", err)
	}
}

// TestDependentEnumOptions_ValidateRejects covers the two validation failures:
// a `when` with neither in nor not_in, and a `when` that names no field while
// the container has no depends_on. Both planes (v3.Validate + strict Validate)
// must reject.
func TestDependentEnumOptions_ValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "when without in or not_in",
			want: "in` or `not_in",
			json: `{
              "apiVersion": "asteby.com/v3", "kind": "Addon",
              "metadata": { "key": "bad", "name": "Bad", "version": "0.1.0" },
              "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
              "models": [ { "key": "Dev", "table": "addon_bad_d", "label": "D", "columns": [
                { "name": "id", "type": "uuid", "primary_key": true },
                { "name": "provider", "type": "text", "depends_on": "type",
                  "options": [ { "value": "qr", "label": "QR", "when": { "field": "type" } } ] }
              ] } ]
            }`,
		},
		{
			name: "when with no field and no depends_on",
			want: "governing sibling field",
			json: `{
              "apiVersion": "asteby.com/v3", "kind": "Addon",
              "metadata": { "key": "bad2", "name": "Bad2", "version": "0.1.0" },
              "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
              "models": [ { "key": "Dev", "table": "addon_bad2_d", "label": "D", "columns": [
                { "name": "id", "type": "uuid", "primary_key": true },
                { "name": "provider", "type": "text",
                  "options": [ { "value": "qr", "label": "QR", "when": { "in": ["whatsapp"] } } ] }
              ] } ]
            }`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// v3 lenient plane.
			err := v3.Validate([]byte(tc.json))
			if err == nil {
				t.Fatalf("v3.Validate accepted invalid manifest %q", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("v3.Validate error = %v, want substring %q", err, tc.want)
			}

			// strict plane (on the parsed + converted manifest, if it parses).
			m, perr := v3.Parse([]byte(tc.json))
			if perr != nil {
				// v3.Parse runs Validate internally, so a parse failure is an
				// acceptable rejection on the strict path too.
				return
			}
			host := manifest.FromV3(m)
			if err := host.Validate("3.0.0"); err == nil {
				t.Errorf("strict Validate accepted invalid manifest %q", tc.name)
			}
		})
	}
}
