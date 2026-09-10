package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const nativeActionJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "connector_whatsapp", "name": "WhatsApp", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [{
      "key": "connect_device",
      "label": "Connect device",
      "target_model": "link_inbox_devices",
      "handler": { "type": "native", "operation": "connect_device" }
    }]
  }
}`

func TestFromV3ActionNativeMapsOperation(t *testing.T) {
	m, err := v3.Parse([]byte(nativeActionJSON))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	defs := legacy.Actions["link_inbox_devices"]
	if len(defs) != 1 || defs[0].Trigger == nil {
		t.Fatalf("native action did not project: %#v", defs)
	}
	trigger := defs[0].Trigger
	if trigger.Type != "native" || trigger.Operation != "connect_device" {
		t.Fatalf("native operation was lost: %#v", trigger)
	}
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("strict validate: %v", err)
	}
}

func TestFromV3ActionNativeRequiresOperation(t *testing.T) {
	bad := strings.Replace(nativeActionJSON, `, "operation": "connect_device"`, "", 1)
	m, err := v3.Parse([]byte(bad))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	if err := legacy.Validate("3.0.0"); err == nil || !strings.Contains(err.Error(), "operation") {
		t.Fatalf("expected operation-required error, got %v", err)
	}
}
