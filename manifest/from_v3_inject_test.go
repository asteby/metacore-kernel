package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const injectManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "shipments", "name": "Shipments", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Shipment",
      "table": "shipments",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "sales_order_id", "type": "uuid" },
        { "name": "status", "type": "text" }
      ]
    }
  ],
  "contributions": {
    "actions": [
      { "key": "buy_label", "target_model": "@commerce.order", "handler": { "type": "wasm", "function": "buy_label" } }
    ],
    "inject": [
      { "key": "shipment_panel", "kind": "panel", "target": "@commerce.order", "slot": "record.tabs", "order": 300,
        "panel": { "key": "shipments", "kind": "relation", "title": "t", "relation": { "via": "Shipment.sales_order_id", "columns": ["status"] } } },
      { "key": "shipping_status", "kind": "column", "target": "@commerce.order",
        "column": { "label": "l", "from": "Shipment", "foreign_key": "sales_order_id", "pick": "latest", "field": "status", "order_by": "updated_at" } }
    ]
  },
  "extension_points": { "sealed": ["Shipment"], "primary_from": ["carriers"] }
}`

func TestFromV3_ProjectsInjectAndValidatesOnLegacyPlane(t *testing.T) {
	m, err := v3.Parse([]byte(injectManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)
	if len(host.Inject) != 2 || host.Inject[0].Target != "@commerce.order" {
		t.Fatalf("inject not projected: %+v", host.Inject)
	}
	if len(host.SealedModels) != 1 || host.SealedModels[0] != "Shipment" || len(host.PrimaryFrom) != 1 {
		t.Fatalf("extension point lists not projected: %+v %+v", host.SealedModels, host.PrimaryFrom)
	}
	if len(host.Actions["@commerce.order"]) != 1 {
		t.Fatalf("role-targeted action must be keyed by its target: %+v", host.Actions)
	}
	if err := host.Validate("3.0.0"); err != nil {
		t.Fatalf("legacy plane must accept what v3 accepts: %v", err)
	}

	// The same rules refuse the same mistakes on the legacy plane.
	low := 100
	host.Inject[0].Order = &low
	if err := host.Validate("3.0.0"); err == nil || !strings.Contains(err.Error(), "order 100 is below 200") {
		t.Fatalf("legacy plane must refuse a contributor order below 200, got: %v", err)
	}
	host.Inject[0].Order = nil
	host.Inject[1].Slot = "record.footer"
	if err := host.Validate("3.0.0"); err == nil || !strings.Contains(err.Error(), "slots@1 catalog") {
		t.Fatalf("legacy plane must refuse a slot outside the catalog, got: %v", err)
	}
	host.Inject[1].Slot = ""
	host.Actions["@commerce"] = host.Actions["@commerce.order"]
	delete(host.Actions, "@commerce.order")
	if err := host.Validate("3.0.0"); err == nil || !strings.Contains(err.Error(), "@<domain>.<entity>") {
		t.Fatalf("legacy plane must refuse a malformed role target, got: %v", err)
	}
}

// "<own addon>.<Model>" is projected under the bare model key the host keys
// own actions by, so the recommended qualified form never makes an own action
// vanish from its model.
func TestFromV3_OwnQualifiedActionTargetProjectsBare(t *testing.T) {
	raw := strings.Replace(injectManifestJSON, `"target_model": "@commerce.order"`, `"target_model": "shipments.Shipment"`, 1)
	m, err := v3.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)
	if len(host.Actions["Shipment"]) != 1 || len(host.Actions["shipments.Shipment"]) != 0 {
		t.Fatalf("own qualified action must be keyed by the bare model: %+v", host.Actions)
	}
	if err := host.Validate("3.0.0"); err != nil {
		t.Fatalf("legacy plane: %v", err)
	}
}
