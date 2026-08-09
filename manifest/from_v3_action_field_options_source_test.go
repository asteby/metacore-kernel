package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// actionFieldOptionsSourceManifestJSON declares an addon whose curated create
// action (placement:create) carries a field that opts into DYNAMIC options via
// options_source: instead of hardcoding a choice list, it names a host-
// registered provider ("connector_repos") that materialises the value/label
// list at metadata-serve time. This is the action-field twin of the column
// options_source contract.
const actionFieldOptionsSourceManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "integration_github", "name": "GitHub", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Issue",
      "table": "addon_github_issues",
      "label": "Issues",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "title", "type": "text" }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "create_issue",
        "label": "New issue",
        "placement": "create",
        "target_model": "Issue",
        "handler": { "type": "wasm", "function": "create_issue" },
        "fields": [
          { "key": "title", "label": "Title", "type": "text", "required": true },
          { "key": "repo", "label": "Repository", "type": "select", "options_source": "connector_repos" }
        ]
      }
    ]
  }
}`

// TestActionFieldOptionsSourceParseAndProject walks options_source on an action
// field through the kernel-side chain: v3.Parse (schema + typed decode) →
// FromV3 (legacy ActionDef.Fields carrier) → JSON round-trip (the shape ops
// serves to the SDK). The host's provider registry then only has to read
// options_source off the served field and materialise the choices.
func TestActionFieldOptionsSourceParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(actionFieldOptionsSourceManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with action field options_source: %v", err)
	}

	// (a) the v3 typed shape carries options_source on the action field.
	af := m.Contributions.Actions[0].Fields
	var repoV3 v3.ActionField
	for _, f := range af {
		if f.Key == "repo" {
			repoV3 = f
		}
	}
	if repoV3.OptionsSource != "connector_repos" {
		t.Errorf("v3 repo field OptionsSource = %q, want connector_repos", repoV3.OptionsSource)
	}

	// (b) FromV3 projects options_source onto the legacy FieldDef carrier.
	host := manifest.FromV3(m)
	defs := host.Actions["Issue"]
	if len(defs) == 0 {
		t.Fatal("FromV3 produced no actions for Issue")
	}
	fields := map[string]manifest.FieldDef{}
	for _, f := range defs[0].Fields {
		fields[f.Key] = f
	}
	if got := fields["repo"].OptionsSource; got != "connector_repos" {
		t.Errorf("legacy repo FieldDef.OptionsSource = %q, want connector_repos", got)
	}

	// (c) the strict (hub/install-time) validation accepts the converted
	// manifest — the "double validation" planes agree.
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// (d) the served JSON carries options_source under the tag the host/SDK
	// read, so the passthrough reaches the frontend.
	raw, err := json.Marshal(fields["repo"])
	if err != nil {
		t.Fatalf("marshal repo field: %v", err)
	}
	if !strings.Contains(string(raw), `"options_source":"connector_repos"`) {
		t.Errorf("served field JSON missing options_source passthrough: %s", raw)
	}
}

// TestV3ActionFieldOptionsSourceBadFormatRejected proves the schema pattern
// gates the provider-key alphabet (lowercase snake only) on action fields.
func TestV3ActionFieldOptionsSourceBadFormatRejected(t *testing.T) {
	bad := strings.Replace(actionFieldOptionsSourceManifestJSON, `"connector_repos"`, `"Connector-Repos"`, 1)
	if err := v3.Validate([]byte(bad)); err == nil {
		t.Fatal("v3.Validate accepted action field options_source with uppercase/dash, want pattern rejection")
	}
}

// TestLegacyValidateActionFieldOptionsSourceFormat proves the strict validator
// enforces the SAME alphabet on action fields as the v3 schema pattern.
func TestLegacyValidateActionFieldOptionsSourceFormat(t *testing.T) {
	mk := func(src string) manifest.Manifest {
		return manifest.Manifest{
			Key:     "integration_github",
			Name:    "GitHub",
			Version: "1.0.0",
			Kernel:  ">=2.0.0",
			Actions: map[string][]manifest.ActionDef{
				"Issue": {{
					Key:  "create_issue",
					Name: "create_issue",
					Fields: []manifest.FieldDef{
						{Key: "repo", Name: "repo", Type: "select", OptionsSource: src},
					},
				}},
			},
		}
	}
	good := mk("connector_repos")
	if err := good.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected valid action field options_source: %v", err)
	}
	bad := mk("Connector-Repos")
	err := bad.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "options_source") {
		t.Fatalf("legacy Validate accepted bad action field options_source, got %v", err)
	}
}
