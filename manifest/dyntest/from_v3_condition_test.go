package dyntest

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// conditionManifestJSON declares a POS addon that contributes a nav entry and
// an action gated on the WORKSHOP addon being installed: an org running POS
// alone must never see them, an org running both must.
const conditionManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "POS", "version": "1.0.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Sale",
      "table": "sales",
      "label": "Sales",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "code", "type": "text" }
      ]
    }
  ],
  "contributions": {
    "navigation": [
      {
        "title": "pos.nav.group",
        "items": [
          { "title": "pos.nav.sales", "model": "Sale" },
          {
            "title": "pos.nav.workshop_intake",
            "model": "Sale",
            "condition": { "addon_installed": "workshop" }
          }
        ]
      },
      {
        "title": "pos.nav.workshop_group",
        "condition": { "addon_installed": "workshop" },
        "items": [ { "title": "pos.nav.bays", "model": "Sale" } ]
      }
    ],
    "actions": [
      {
        "key": "send_to_workshop",
        "label": "Send to workshop",
        "target_model": "Sale",
        "handler": { "type": "webhook" },
        "condition": { "addon_installed": "workshop" }
      }
    ]
  }
}`

// TestConditionParseAndProject walks contributions[].condition through the
// kernel chain: v3.Parse (strict jsonschema + typed decode) → FromV3 (host
// carrier) → legacy Validate.
func TestConditionParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(conditionManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with contribution conditions: %v", err)
	}
	if got := m.Contributions.Navigation[1].Condition.AddonInstalled; got != "workshop" {
		t.Errorf("v3 nav group condition = %q, want workshop", got)
	}
	if got := m.Contributions.Navigation[0].Items[1].Condition.AddonInstalled; got != "workshop" {
		t.Errorf("v3 nav item condition = %q, want workshop", got)
	}
	if m.Contributions.Navigation[0].Items[0].Condition != nil {
		t.Errorf("unconditional nav item got a condition")
	}
	if got := m.Contributions.Actions[0].Condition.AddonInstalled; got != "workshop" {
		t.Errorf("v3 action condition = %q, want workshop", got)
	}

	host := manifest.FromV3(m)
	if got := host.Navigation[1].Condition.AddonInstalled; got != "workshop" {
		t.Errorf("host nav group condition = %q, want workshop", got)
	}
	if got := host.Navigation[0].Items[1].Condition.AddonInstalled; got != "workshop" {
		t.Errorf("host nav item condition = %q, want workshop", got)
	}
	if host.Navigation[0].Items[0].Condition != nil {
		t.Errorf("host unconditional nav item got a condition")
	}
	actions := host.Actions["Sale"]
	if len(actions) != 1 || actions[0].Condition == nil || actions[0].Condition.AddonInstalled != "workshop" {
		t.Fatalf("host action condition not carried: %+v", actions)
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}
}

// TestConditionSatisfied covers the predicate itself, including the two
// permissive defaults: no predicate, and a caller that cannot resolve
// installation state (nil resolver) — neither may hide a contribution.
func TestConditionSatisfied(t *testing.T) {
	installed := func(key string) bool { return key == "workshop" }

	cases := []struct {
		name string
		cond *manifest.ConditionDef
		fn   func(string) bool
		want bool
	}{
		{"nil condition", nil, installed, true},
		{"empty predicate", &manifest.ConditionDef{}, installed, true},
		{"installed", &manifest.ConditionDef{AddonInstalled: "workshop"}, installed, true},
		{"not installed", &manifest.ConditionDef{AddonInstalled: "fiscal_mexico"}, installed, false},
		{"nil resolver", &manifest.ConditionDef{AddonInstalled: "fiscal_mexico"}, nil, true},
	}
	for _, tc := range cases {
		if got := tc.cond.Satisfied(tc.fn); got != tc.want {
			t.Errorf("%s: Satisfied = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestConditionRejectedWhenEmpty asserts an empty `condition` block is an
// authoring error, not a silent no-op.
func TestConditionRejectedWhenEmpty(t *testing.T) {
	bad := strings.Replace(conditionManifestJSON, `"condition": { "addon_installed": "workshop" }`, `"condition": {}`, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted an empty condition block")
	}
}

// TestConditionRejectedWhenKeyMalformed asserts the addon key is validated.
func TestConditionRejectedWhenKeyMalformed(t *testing.T) {
	bad := strings.Replace(conditionManifestJSON, `"addon_installed": "workshop"`, `"addon_installed": "Workshop Pro"`, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted a malformed addon_installed key")
	}
}
