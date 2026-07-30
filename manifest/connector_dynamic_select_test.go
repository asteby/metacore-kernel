package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// A token connector whose serie credential is a dynamic_select fed by a WASM
// export, grouped into a two-step wizard, with a test-connection export.
const connectorDynamicSelectJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "fiscal_mexico", "name": "Timbrado CFDI", "version": "0.6.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "connectors": [
    {
      "key": "factura_com",
      "label": "fiscal_mexico.connector.factura_com",
      "auth": "token",
      "test_export": "handle_connector_test",
      "form_layout": {
        "mode": "steps",
        "sections": [
          { "key": "creds", "title": "Credenciales" },
          { "key": "series", "title": "Series" }
        ]
      },
      "credentials": [
        { "key": "api_key", "type": "secret", "required": true, "section": "creds" },
        { "key": "api_secret", "type": "secret", "required": true, "section": "creds" },
        {
          "key": "serie_invoice",
          "type": "dynamic_select",
          "options_source": "handle_connector_lookup_series",
          "section": "series",
          "required": false
        }
      ]
    }
  ]
}`

func TestConnectorDynamicSelect_ParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(connectorDynamicSelectJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected dynamic_select connector: %v", err)
	}
	c := m.Connectors[0]
	if c.TestExport != "handle_connector_test" {
		t.Errorf("v3 TestExport = %q", c.TestExport)
	}
	if c.FormLayout == nil || c.FormLayout.Mode != "steps" || len(c.FormLayout.Sections) != 2 {
		t.Fatalf("v3 FormLayout = %+v", c.FormLayout)
	}
	var serie *v3.Setting
	for i := range c.Credentials {
		if c.Credentials[i].Key == "serie_invoice" {
			serie = &c.Credentials[i]
		}
	}
	if serie == nil || serie.Type != "dynamic_select" || serie.OptionsSource != "handle_connector_lookup_series" {
		t.Fatalf("v3 serie credential = %+v", serie)
	}

	// Projection must carry the new fields through to what ops reads.
	out := manifest.FromV3(m)
	oc := out.Connectors[0]
	if oc.TestExport != "handle_connector_test" {
		t.Errorf("projected TestExport = %q", oc.TestExport)
	}
	if oc.FormLayout == nil || oc.FormLayout.Mode != "steps" {
		t.Errorf("projected FormLayout = %+v", oc.FormLayout)
	}
	var pcred *manifest.CredentialDef
	for i := range oc.Credentials {
		if oc.Credentials[i].Key == "serie_invoice" {
			pcred = &oc.Credentials[i]
		}
	}
	if pcred == nil || pcred.Type != "dynamic_select" || pcred.OptionsSource != "handle_connector_lookup_series" {
		t.Fatalf("projected serie credential = %+v", pcred)
	}

	// The lookup + test exports MUST land in the backend whitelist, or the wasm
	// runtime rejects them as un-declared exports.
	if out.Backend == nil {
		t.Fatal("expected a wasm Backend derived from the connector exports")
	}
	hasExport := func(name string) bool {
		for _, e := range out.Backend.Exports {
			if e == name {
				return true
			}
		}
		return false
	}
	if !hasExport("handle_connector_test") {
		t.Errorf("Backend.Exports %v missing test_export handle_connector_test", out.Backend.Exports)
	}
	if !hasExport("handle_connector_lookup_series") {
		t.Errorf("Backend.Exports %v missing options_source handle_connector_lookup_series", out.Backend.Exports)
	}
}
