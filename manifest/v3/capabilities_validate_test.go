package v3

import (
	"strings"
	"testing"
)

func capabilityManifest(key, extra string) []byte {
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "` + key + `", "name": "X", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      ` + extra + `
    }`)
}

// A provider (connector_whatsapp's native sidecar) and a consumer (crm's
// send_whatsapp) both parse; neither names the other.
func TestCapabilityProviderAndConsumerParse(t *testing.T) {
	provider := capabilityManifest("connector_whatsapp", `"provides_capabilities": [{
        "key": "messaging.whatsapp.send",
        "label": "WhatsApp (QR)",
        "handler": {"type": "native", "operation": "whatsapp_message_send"},
        "input": {"message": "payload.message", "device_id": "payload.device_id"}
      }]`)
	m, err := Parse(provider)
	if err != nil {
		t.Fatalf("provider Parse: %v", err)
	}
	if len(m.ProvidesCapabilities) != 1 || m.ProvidesCapabilities[0].Handler.Operation != "whatsapp_message_send" {
		t.Fatalf("provides_capabilities not parsed: %+v", m.ProvidesCapabilities)
	}

	consumer := capabilityManifest("crm_lite", `"contributions": {"actions": [{
        "key": "send_whatsapp", "label": "Send",
        "handler": {"type": "capability", "capability": "messaging.whatsapp.send",
          "input": {"to": "record.phone", "message": "payload.text"}}
      }]}`)
	cm, err := Parse(consumer)
	if err != nil {
		t.Fatalf("consumer Parse: %v", err)
	}
	h := cm.Contributions.Actions[0].Handler
	if h.Capability != "messaging.whatsapp.send" || h.Input["to"] != "record.phone" {
		t.Fatalf("capability handler not parsed: %+v", h)
	}
}

func TestCapabilityValidationRejectsBadDeclarations(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"provider handler type": {`"provides_capabilities": [{"key": "messaging.whatsapp.send", "handler": {"type": "connector"}}]`, "handler"},
		"provider reads unknown contract field": {`"provides_capabilities": [{"key": "messaging.whatsapp.send", "handler": {"type": "wasm", "function": "send"}, "input": {"phone": "payload.phone"}}]`, `not a field of capability`},
		"duplicated provider key": {`"provides_capabilities": [{"key": "messaging.whatsapp.send", "handler": {"type": "wasm", "function": "a"}}, {"key": "messaging.whatsapp.send", "handler": {"type": "wasm", "function": "b"}}]`, "duplicated"},
		"consumer maps a non-contract field": {`"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "capability", "capability": "messaging.whatsapp.send", "input": {"phone": "record.phone"}}}]}`, `"phone" is not a field`},
		"consumer mixes connector": {`"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "capability", "capability": "messaging.whatsapp.send", "connector": "link"}}]}`, "cannot declare"},
		"capability outside type": {`"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "wasm", "function": "a", "capability": "messaging.whatsapp.send"}}]}`, "only allowed when type=capability"},
	}
	for name, tc := range cases {
		_, err := Parse(capabilityManifest("xaddon", tc.extra))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}
