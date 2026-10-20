package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const connectorInputRulesJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "mercadopago", "name": "Mercado Pago", "version": "0.9.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
  "settings": [
    { "key": "notification_url", "type": "string",
      "label": "mercadopago.settings.notification_url",
      "description": "mercadopago.settings.notification_url_help",
      "input_rules": { "format": "url_https" } }
  ],
  "connectors": [
    {
      "key": "mercadopago",
      "auth": "token",
      "credentials": [
        { "key": "access_token", "type": "secret", "required": true,
          "input_rules": {
            "trim": true,
            "pattern": "^(APP_USR|TEST)-[0-9]{8,}-[0-9]{4,}-[0-9a-f]{32}-[0-9]+$",
            "pattern_hint": "mercadopago.connector.hint.access_token",
            "examples": ["APP_USR-1234567890123456-101912-0123456789abcdef0123456789abcdef-123456789"],
            "max_length": 200,
            "autocomplete": "new-password",
            "mask": "full",
            "forbid_patterns": [
              { "pattern": "^(APP_USR|TEST)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$",
                "message": "mercadopago.connector.error.public_key_in_access_token" }
            ]
          } },
        { "key": "public_key", "type": "string",
          "input_rules": { "pattern": "^(APP_USR|TEST)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" } }
      ]
    }
  ]
}`

func TestConnectorInputRules_ParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(connectorInputRulesJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected input_rules: %v", err)
	}
	r := m.Connectors[0].Credentials[0].InputRules
	if r == nil || !r.TrimEnabled() || len(r.ForbidPatterns) != 1 || *r.MaxLength != 200 {
		t.Fatalf("v3 input_rules = %+v", r)
	}
	out := manifest.FromV3(m)
	cr := out.Connectors[0].Credentials[0]
	if cr.InputRules == nil || cr.InputRules.Pattern != r.Pattern || cr.InputRules.Autocomplete != "new-password" {
		t.Fatalf("projected credential input_rules = %+v", cr.InputRules)
	}
	if out.Connectors[0].Credentials[1].InputRules == nil {
		t.Fatal("public_key input_rules dropped")
	}
	s := out.Settings[0]
	if s.Description != "mercadopago.settings.notification_url_help" || s.InputRules == nil || s.InputRules.Format != "url_https" {
		t.Fatalf("projected setting = %+v", s)
	}
	// JSON round-trip keeps the block under the same tag the host reads.
	raw, _ := json.Marshal(out.Connectors[0].Credentials[0])
	if !strings.Contains(string(raw), `"input_rules":{`) || !strings.Contains(string(raw), `"forbid_patterns"`) {
		t.Fatalf("credential JSON = %s", raw)
	}
}

func TestConnectorInputRules_ManifestRejectsBadRules(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"hostile regex": {`"^(APP_USR|TEST)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" } }`,
			`"^(a+)+$" } }`, "connectors[mercadopago].credentials[public_key].input_rules.pattern"},
		"inline flag": {`"^(APP_USR|TEST)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" } }`,
			`"(?i)^app_usr-.*$" } }`, "inline flag"},
		"example breaks its rules": {`"APP_USR-1234567890123456-101912-0123456789abcdef0123456789abcdef-123456789"`,
			`"APP_USR-0a1b2c3d-4e5f-6789-abcd-ef0123456789"`, "examples[0]"},
		"unknown format (schema)": {`"format": "url_https"`, `"format": "ip"`, "schema"},
		"unknown key (closed schema)": {`"trim": true,`, `"trim": true, "regex": "x",`, "schema"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(connectorInputRulesJSON, tc.from, tc.to, 1)
			if doc == connectorInputRulesJSON {
				t.Fatal("replacement did not apply")
			}
			_, err := v3.Parse([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// Manifests without input_rules keep validating and projecting exactly as
// before (nil block, no new JSON keys).
func TestConnectorInputRules_AbsentIsNoop(t *testing.T) {
	m, err := v3.Parse([]byte(connectorDynamicSelectJSON))
	if err != nil {
		t.Fatal(err)
	}
	out := manifest.FromV3(m)
	raw, _ := json.Marshal(out.Connectors)
	if strings.Contains(string(raw), "input_rules") {
		t.Fatalf("unexpected input_rules in %s", raw)
	}
}

func TestActionFieldInputRules(t *testing.T) {
	doc := `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "demo", "name": "Demo", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
  "contributions": { "actions": [ {
    "key": "link_account", "label": "demo.action.link",
    "handler": { "type": "wasm", "function": "handle_link" },
    "fields": [ { "key": "account_id", "type": "text", "input_rules": { "pattern": "PATTERN" } } ]
  } ] }
}`
	m, err := v3.Parse([]byte(strings.Replace(doc, "PATTERN", `^[0-9]{6,12}$`, 1)))
	if err != nil {
		t.Fatalf("valid action field rules rejected: %v", err)
	}
	out := manifest.FromV3(m)
	var got *manifest.FieldDef
	for _, acts := range out.Actions {
		for i := range acts {
			for j := range acts[i].Fields {
				got = &acts[i].Fields[j]
			}
		}
	}
	if got == nil || got.InputRules == nil || got.InputRules.Pattern != `^[0-9]{6,12}$` {
		t.Fatalf("action field input_rules not projected: %+v", got)
	}
	_, err = v3.Parse([]byte(strings.Replace(doc, "PATTERN", `^(x*)*$`, 1)))
	if err == nil || !strings.Contains(err.Error(), "contributions.actions[link_account].fields[account_id].input_rules.pattern") {
		t.Fatalf("hostile action field pattern: %v", err)
	}
}
