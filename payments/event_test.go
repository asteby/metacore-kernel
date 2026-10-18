package payments

import (
	"errors"
	"testing"
)

func TestDecodeConfirmed(t *testing.T) {
	// The shape mercadopago 0.8.0 publishes (organization_id and occurred_at
	// at the top level), with the host-stamped emitter.
	mp := `{"provider":"mercadopago","emitter":"mercadopago","event":"commerce.payments.confirmed",
	  "organization_id":"8d0c","occurred_at":"2026-10-18T10:00:00Z",
	  "result":{"order_ref":"SO-1","status":"paid","amount":"1499.00","currency":"MXN","provider_ref":"123","event_id":"evt-1","kind":"payment"}}`
	c, err := DecodeConfirmed([]byte(mp))
	if err != nil || c.Result.Status != StatusPaid || c.Result.DedupeEventID() != "evt-1" {
		t.Fatalf("mercadopago payload: %+v %v", c, err)
	}
	terminal := `{"provider":"clip","emitter":"clip","source":"poll",
	  "result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9",
	    "channel":"terminal","device_id":"PIN-1","card":{"type":"credit","brand":"visa","last4":"0007"}}}`
	c, err = DecodeConfirmed([]byte(terminal))
	if err != nil || c.Result.Card == nil || c.Result.Card.Last4 != "0007" || c.Result.Channel != ChannelTerminal {
		t.Fatalf("terminal payload: %+v %v", c, err)
	}
	// Re-polling an unchanged payment derives the same id: a duplicate.
	if c.Result.DedupeEventID() != DerivedEventID(KindPayment, "tx9", StatusPaid, "") {
		t.Fatalf("derived id %q", c.Result.DedupeEventID())
	}
	for name, bad := range map[string]string{
		"no emitter":     `{"provider":"clip","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
		"impersonation":  `{"provider":"mercadopago","emitter":"clip","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
		"raw status":     `{"provider":"clip","emitter":"clip","result":{"order_ref":"SO-2","status":"approved","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
		"float money":    `{"provider":"clip","emitter":"clip","result":{"order_ref":"SO-2","status":"paid","amount":80,"currency":"MXN","provider_ref":"tx9"}}`,
		"bad channel":    `{"provider":"clip","emitter":"clip","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9","channel":"pos"}}`,
		"unknown source": `{"provider":"clip","emitter":"clip","source":"guess","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
	} {
		if _, err := DecodeConfirmed([]byte(bad)); !errors.Is(err, ErrUntrustedConfirmation) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestDedupeKey(t *testing.T) {
	e := PaymentEvent{Provider: "mercadopago", EventID: "1"}
	if e.DedupeKey() == DedupeKey("clip", "1") || e.DedupeKey() != DedupeKey("mercadopago", "1") {
		t.Fatal("dedupe key must be per provider")
	}
	if DerivedEventID("", "r", StatusPartiallyRefunded, "10.00") == DerivedEventID("", "r", StatusPartiallyRefunded, "20.00") {
		t.Fatal("a second partial refund must not dedupe against the first")
	}
}
