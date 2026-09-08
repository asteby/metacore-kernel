package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// connectorConditionManifestJSON declares a fiscal addon whose stamp action
// needs BOTH a connected PAC (org-level, resolved by the host) and an
// unstamped invoice (record-level, resolved per row by the client).
const connectorConditionManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "fiscal_mexico", "name": "CFDI", "version": "1.0.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "connectors": [
    { "key": "factura_com", "label": "factura.com", "auth": "token",
      "credentials": [ { "key": "api_key", "type": "secret", "required": true } ] }
  ],
  "models": [
    {
      "key": "Invoice",
      "table": "invoices",
      "label": "Invoices",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "fiscal_uuid", "type": "text" }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "stamp_fiscal",
        "label": "Timbrar",
        "target_model": "Invoice",
        "handler": { "type": "wasm", "export": "stamp" },
        "condition": { "connector_connected": "factura_com", "field": "fiscal_uuid", "operator": "falsy" }
      }
    ]
  }
}`

// The gate is worthless if it does not survive the v3 → host conversion: the
// host is the only layer that can answer "is this connector connected for this
// org?", so the projected ActionDef must still carry the predicate.
func TestConnectorConditionSurvivesProjection(t *testing.T) {
	m3, err := v3.Parse([]byte(connectorConditionManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m3)

	actions := host.Actions["Invoice"]
	var stamp *manifest.ActionDef
	for i := range actions {
		if actions[i].Key == "stamp_fiscal" {
			stamp = &actions[i]
		}
	}
	if stamp == nil {
		t.Fatal("stamp_fiscal did not survive the projection")
	}
	if stamp.Condition == nil {
		t.Fatal("action condition was dropped by the projection")
	}
	if stamp.Condition.ConnectorConnected != "factura_com" {
		t.Fatalf("connector gate not projected: %+v", stamp.Condition)
	}
	if got := stamp.Condition.UnmetPolicy(); got != v3.UnmetDisable {
		t.Fatalf("projected unmet policy = %q, want %q (a connector gate is remediable)", got, v3.UnmetDisable)
	}

	connected := func(k string) bool { return false }
	if stamp.Condition.SatisfiedBy(nil, connected) {
		t.Fatal("an unconnected PAC must not satisfy the gate")
	}
	if !stamp.Condition.SatisfiedBy(nil, func(string) bool { return true }) {
		t.Fatal("a connected PAC must satisfy the gate")
	}
}
