package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// A cross-addon connector action: an addon action drives ANOTHER addon's
// connector export (here crm-lite's "Send WhatsApp" runs the `link` connector's
// link_send_message export) without owning or duplicating that connector.
const connectorActionJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "crm_lite", "name": "CRM", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      {
        "key": "send_whatsapp",
        "label": "Send WhatsApp",
        "target_model": "crm_leads",
        "handler": { "type": "connector", "connector": "link", "export": "link_send_message" }
      }
    ]
  }
}`

func TestFromV3_ActionConnector_MapsThrough(t *testing.T) {
	m, err := v3.Parse([]byte(connectorActionJSON))
	if err != nil {
		t.Fatalf("v3 parse (schema must accept handler.type=connector): %v", err)
	}
	legacy := manifest.FromV3(m)
	defs := legacy.Actions["crm_leads"]
	if len(defs) != 1 {
		t.Fatalf("want 1 action, got %d", len(defs))
	}
	tr := defs[0].Trigger
	if tr == nil || tr.Type != "connector" {
		t.Fatalf("trigger did not map to connector: %+v", tr)
	}
	if tr.Connector != "link" || tr.Export != "link_send_message" {
		t.Fatalf("connector/export did not map through: %+v", tr)
	}
	// Dual-validation parity: the strict legacy validator must also accept it,
	// and must NOT cross-check the export against this addon's backend exports
	// (the export lives in the connector-owning addon).
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("strict validate rejected a valid connector action: %v", err)
	}
}

func TestFromV3_ActionConnector_SchemaRejectsMissingConnector(t *testing.T) {
	// handler.type=connector without a connector key: the strict validator
	// rejects it (the schema keeps connector optional to stay shared with other
	// handler shapes, so the semantic gate lives in Validate).
	bad := strings.Replace(connectorActionJSON,
		`"handler": { "type": "connector", "connector": "link", "export": "link_send_message" }`,
		`"handler": { "type": "connector", "export": "link_send_message" }`, 1)
	m, err := v3.Parse([]byte(bad))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	if err := legacy.Validate("3.0.0"); err == nil ||
		!strings.Contains(err.Error(), "connector") {
		t.Fatalf("expected connector-required error, got %v", err)
	}
}
