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
		"provider handler type":                 {`"provides_capabilities": [{"key": "messaging.whatsapp.send", "handler": {"type": "connector"}}]`, "handler"},
		"provider reads unknown contract field": {`"provides_capabilities": [{"key": "messaging.whatsapp.send", "handler": {"type": "wasm", "function": "send"}, "input": {"phone": "payload.phone"}}]`, `not a field of capability`},
		"duplicated provider key":               {`"provides_capabilities": [{"key": "messaging.whatsapp.send", "handler": {"type": "wasm", "function": "a"}}, {"key": "messaging.whatsapp.send", "handler": {"type": "wasm", "function": "b"}}]`, "duplicated"},
		"consumer maps a non-contract field":    {`"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "capability", "capability": "messaging.whatsapp.send", "input": {"phone": "record.phone"}}}]}`, `"phone" is not a field`},
		"consumer mixes connector":              {`"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "capability", "capability": "messaging.whatsapp.send", "connector": "link"}}]}`, "cannot declare"},
		"lower-case country":                    {`"provides_capabilities": [{"key": "fiscal.invoice.create", "handler": {"type": "wasm", "function": "a"}, "countries": ["mx"]}]`, "countries"},
		"duplicated country":                    {`"provides_capabilities": [{"key": "fiscal.invoice.create", "handler": {"type": "wasm", "function": "a"}, "countries": ["MX", "MX"]}]`, "countries"},
		"provider reads unknown commerce field": {`"provides_capabilities": [{"key": "commerce.shipping.rates", "handler": {"type": "wasm", "function": "a"}, "input": {"cp": "payload.zip"}}]`, `not a field of capability`},
		"capability outside type":               {`"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "wasm", "function": "a", "capability": "messaging.whatsapp.send"}}]}`, "only allowed when type=capability"},
	}
	for name, tc := range cases {
		_, err := Parse(capabilityManifest("xaddon", tc.extra))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

// A commerce query provider declares the countries it serves; a consumer
// maps contract fields of a query contract like any other.
func TestCapabilityQueryProviderWithCountries(t *testing.T) {
	m, err := Parse(capabilityManifest("factura_com", `"provides_capabilities": [{
        "key": "fiscal.invoice.create", "label": "Factura.com (CFDI 4.0)",
        "handler": {"type": "wasm", "function": "invoice_create"},
        "input": {"rfc": "payload.customer"},
        "countries": ["MX"]
      }],
      "contributions": {"actions": [{"key": "quote", "label": "Cotizar", "handler": {"type": "capability",
        "capability": "commerce.shipping.rates", "input": {"destination": "payload.address", "currency": "const:MXN"}}}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := m.ProvidesCapabilities[0].Countries; len(got) != 1 || got[0] != "MX" {
		t.Fatalf("countries = %v", got)
	}
}

// r4 I6: marketplace routing and commission are server-derived; a payments
// handler never maps them from a payload (the buyer would set the fee).
func TestPaymentsSellerNeverFromPayload(t *testing.T) {
	cases := map[string]string{
		"consumer maps seller fee": `"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "capability", "capability": "commerce.payments.session", "input": {"seller.application_fee": "payload.fee"}}}]}`,
		"consumer maps seller":     `"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "capability", "capability": "commerce.payments.session", "input": {"seller": "payload.seller"}}}]}`,
		"provider maps refund fee": `"provides_capabilities": [{"key": "commerce.payments.refund", "handler": {"type": "wasm", "function": "a"}, "input": {"refund_application_fee": "payload.refund_application_fee"}}]`,
	}
	for name, extra := range cases {
		_, err := Parse(capabilityManifest("xaddon", extra))
		if err == nil || !strings.Contains(err.Error(), "server-derived") {
			t.Errorf("%s: want server-derived error, got %v", name, err)
		}
	}
	// From the stored record it is fine.
	ok := `"contributions": {"actions": [{"key": "a", "label": "A", "handler": {"type": "capability", "capability": "commerce.payments.session", "input": {"seller": "record.seller"}}}]}`
	if _, err := Parse(capabilityManifest("xaddon", ok)); err != nil && strings.Contains(err.Error(), "server-derived") {
		t.Fatalf("record-derived seller rejected: %v", err)
	}
}
