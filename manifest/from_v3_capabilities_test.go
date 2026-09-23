package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// The capability dispatch target must survive the v3 -> host projection on
// both sides, or the host can never route (the same failure mode the
// connector trigger had before modelbase carried it).
func TestFromV3ProjectsCapabilities(t *testing.T) {
	m := FromV3(&v3.Manifest{
		Metadata: v3.Metadata{Key: "connector_whatsapp", Name: "WA", Version: "1.0.0"},
		ProvidesCapabilities: []v3.ProvidedCapability{{
			Key: "messaging.whatsapp.send", Label: "WhatsApp (QR)",
			Handler: v3.Handler{Type: "native", Operation: "whatsapp_message_send"},
			Input:   map[string]string{"message": "payload.message"},
		}},
		Contributions: &v3.Contributions{Actions: []v3.Action{{
			Key: "send_whatsapp", Label: "Send", TargetModel: "leads",
			Handler: v3.Handler{Type: "capability", Capability: "messaging.whatsapp.send", Input: map[string]string{"to": "record.phone"}},
		}}},
	})
	if len(m.ProvidesCapabilities) != 1 {
		t.Fatalf("want 1 provided capability, got %d", len(m.ProvidesCapabilities))
	}
	pc := m.ProvidesCapabilities[0]
	if pc.Trigger == nil || pc.Trigger.Type != "native" || pc.Trigger.Operation != "whatsapp_message_send" || pc.Input["message"] != "payload.message" {
		t.Fatalf("provider projection lost its target: %+v", pc)
	}
	var trig *ActionTrigger
	for _, defs := range m.Actions {
		for _, d := range defs {
			if d.Key == "send_whatsapp" {
				trig = d.Trigger
			}
		}
	}
	if trig == nil || trig.Type != "capability" || trig.Capability != "messaging.whatsapp.send" || trig.Input["to"] != "record.phone" {
		t.Fatalf("consumer trigger lost: %+v", trig)
	}
	if err := validateActionTrigger(trig, nil); err != nil {
		t.Fatalf("capability trigger must validate: %v", err)
	}
	if err := validateActionTrigger(&ActionTrigger{Type: "capability", Capability: "messaging.whatsapp.send", Connector: "link"}, nil); err == nil {
		t.Fatal("capability trigger must not also name a connector")
	}
}

// A wasm capability provider's function must be in backend.exports, or the
// runtime rejects the dispatch with "not in backend.exports".
func TestFromV3WhitelistsWasmCapabilityProvider(t *testing.T) {
	m := FromV3(&v3.Manifest{
		Metadata: v3.Metadata{Key: "whatsapp", Name: "Link", Version: "1.0.0"},
		ProvidesCapabilities: []v3.ProvidedCapability{{
			Key: "messaging.whatsapp.send", Handler: v3.Handler{Type: "wasm", Function: "link_send_message"},
		}},
	})
	if m.Backend == nil {
		t.Fatal("a wasm capability provider must derive a wasm backend")
	}
	found := false
	for _, e := range m.Backend.Exports {
		if e == "link_send_message" {
			found = true
		}
	}
	if !found {
		t.Fatalf("link_send_message missing from exports: %v", m.Backend.Exports)
	}
}
