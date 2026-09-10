package modelbase

import (
	"encoding/json"
	"testing"
)

// The served ActionDef must round-trip a `connector` trigger: the host builds
// modelbase.ActionDef from the kernel manifest via json.Marshal(km.Actions) →
// json.Unmarshal(&rec.Actions) (KernelManifestToRecord), so the JSON tags here
// must match manifest.ActionTrigger or the cross-addon connector dispatch is
// silently dropped before it reaches the host action registry.
func TestActionDef_ConnectorTriggerRoundTrips(t *testing.T) {
	// The exact JSON manifest.ActionDef marshals for a connector action.
	in := `{"key":"send_whatsapp","name":"send_whatsapp","label":"Send WhatsApp",` +
		`"trigger":{"type":"connector","connector":"link","export":"link_send_message"}}`

	var def ActionDef
	if err := json.Unmarshal([]byte(in), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.Trigger == nil {
		t.Fatal("trigger dropped on unmarshal — the served ActionDef lost the connector dispatch")
	}
	if def.Trigger.Type != "connector" {
		t.Errorf("type = %q, want connector", def.Trigger.Type)
	}
	if def.Trigger.Connector != "link" {
		t.Errorf("connector = %q, want link", def.Trigger.Connector)
	}
	if def.Trigger.Export != "link_send_message" {
		t.Errorf("export = %q, want link_send_message", def.Trigger.Export)
	}

	// And it must survive a re-marshal (host persists rec.Actions as jsonb).
	out, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var again ActionDef
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if again.Trigger == nil || again.Trigger.Connector != "link" {
		t.Fatalf("connector trigger lost across re-marshal: %s", out)
	}
}

func TestActionTriggerPreservesNativeOperation(t *testing.T) {
	raw := []byte(`{"key":"connect","name":"connect","label":"Connect","trigger":{"type":"native","operation":"connect_device"}}`)
	var action ActionDef
	if err := json.Unmarshal(raw, &action); err != nil {
		t.Fatal(err)
	}
	if action.Trigger == nil || action.Trigger.Type != "native" || action.Trigger.Operation != "connect_device" {
		t.Fatalf("native trigger projection lost operation: %#v", action.Trigger)
	}
	roundTrip, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err := json.Unmarshal(roundTrip, &projected); err != nil {
		t.Fatal(err)
	}
	trigger := projected["trigger"].(map[string]any)
	if trigger["operation"] != "connect_device" {
		t.Fatalf("served action lost native operation: %s", roundTrip)
	}
}
