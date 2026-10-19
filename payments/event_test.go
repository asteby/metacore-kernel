package payments

import (
	"errors"
	"testing"
)

// providers is the org's set of commerce.payments.* providers.
func providers(addons ...string) func(string) bool {
	return func(a string) bool {
		for _, x := range addons {
			if x == a {
				return true
			}
		}
		return false
	}
}

func TestDecodeConfirmed(t *testing.T) {
	isProvider := providers("mercadopago", "clip")
	// The shape mercadopago 0.8.0 publishes (organization_id and occurred_at
	// at the top level), with the host-stamped emitter.
	mp := `{"provider":"mercadopago","emitter":"mercadopago","event":"commerce.payments.confirmed",
	  "organization_id":"8d0c","occurred_at":"2026-10-18T10:00:00Z",
	  "result":{"order_ref":"SO-1","status":"paid","amount":"1499.00","currency":"MXN","provider_ref":"123","event_id":"evt-1","kind":"payment"}}`
	c, err := DecodeConfirmed([]byte(mp), isProvider)
	if err != nil || c.Result.Status != StatusPaid || c.Result.DedupeEventID() != "evt-1" {
		t.Fatalf("mercadopago payload: %+v %v", c, err)
	}
	terminal := `{"provider":"clip","emitter":"clip","source":"poll",
	  "result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9",
	    "channel":"terminal","device_id":"PIN-1","card":{"type":"credit","brand":"visa","last4":"0007"}}}`
	c, err = DecodeConfirmed([]byte(terminal), isProvider)
	if err != nil || c.Result.Card == nil || c.Result.Card.Last4 != "0007" || c.Result.Channel != ChannelTerminal {
		t.Fatalf("terminal payload: %+v %v", c, err)
	}
	// Re-polling an unchanged payment derives the same id: a duplicate.
	if c.Result.DedupeEventID() != DerivedEventID(KindPayment, "tx9", StatusPaid, "MXN", "80", "") {
		t.Fatalf("derived id %q", c.Result.DedupeEventID())
	}
	for name, bad := range map[string]string{
		"no emitter":     `{"provider":"clip","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
		"impersonation":  `{"provider":"mercadopago","emitter":"clip","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
		"raw status":     `{"provider":"clip","emitter":"clip","result":{"order_ref":"SO-2","status":"approved","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
		"float money":    `{"provider":"clip","emitter":"clip","result":{"order_ref":"SO-2","status":"paid","amount":80,"currency":"MXN","provider_ref":"tx9"}}`,
		"bad channel":    `{"provider":"clip","emitter":"clip","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9","channel":"pos"}}`,
		"unknown source": `{"provider":"clip","emitter":"clip","source":"guess","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
		// r4 I1: a channel naming itself provider (EmitPolicy warn) is not one.
		"channel as provider": `{"provider":"pos","emitter":"pos","result":{"order_ref":"SO-2","status":"paid","amount":"80","currency":"MXN","provider_ref":"tx9"}}`,
	} {
		if _, err := DecodeConfirmed([]byte(bad), isProvider); !errors.Is(err, ErrUntrustedConfirmation) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestDecodeConfirmedNeedsProviderCheck(t *testing.T) {
	mp := `{"provider":"mercadopago","emitter":"mercadopago","result":{"order_ref":"SO-1","status":"paid","amount":"10","currency":"MXN","provider_ref":"1"}}`
	if _, err := DecodeConfirmed([]byte(mp), nil); !errors.Is(err, ErrUntrustedConfirmation) {
		t.Fatalf("nil provider check accepted: %v", err)
	}
}

// r1 punto 5: a legitimate provider cannot confirm another provider's payment.
func TestConfirmedFor(t *testing.T) {
	p := validPayment() // mercadopago, ORD01, p1
	ok := Confirmed{Provider: "mercadopago", PaymentID: "p1", Result: Outcome{ProviderRef: "ORD01"}}
	if err := ok.ConfirmedFor(p); err != nil {
		t.Fatalf("own payment: %v", err)
	}
	for name, c := range map[string]Confirmed{
		"other provider":     {Provider: "clip", PaymentID: "p1", Result: Outcome{ProviderRef: "ORD01"}},
		"other payment":      {Provider: "mercadopago", PaymentID: "p2", Result: Outcome{ProviderRef: "ORD01"}},
		"other provider ref": {Provider: "mercadopago", PaymentID: "p1", Result: Outcome{ProviderRef: "ORD02"}},
	} {
		if err := c.ConfirmedFor(p); !errors.Is(err, ErrUntrustedConfirmation) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestDedupeKey(t *testing.T) {
	e := PaymentEvent{Provider: "mercadopago", EventID: "1"}
	if e.DedupeKey() == DedupeKey("clip", "1") || e.DedupeKey() != DedupeKey("mercadopago", "1") {
		t.Fatal("dedupe key must be per provider")
	}
	if DerivedEventID("", "r", StatusPartiallyRefunded, "MXN", "", "10.00") == DerivedEventID("", "r", StatusPartiallyRefunded, "MXN", "", "20.00") {
		t.Fatal("a second partial refund must not dedupe against the first")
	}
	if DerivedEventID("", "r", StatusPaid, "MXN", "20.0", "") != DerivedEventID(KindPayment, "r", StatusPaid, "mxn", "20.00", "") {
		t.Fatal("the same state must derive the same id")
	}
}

// r4 #507 I1: two partial refunds without event id nor refunded total (30,
// then 20) are two updates; the refunded total ends at 50.
func TestDerivedEventIDPartialRefundsByAmount(t *testing.T) {
	stored := Payment{Status: StatusPaid, Amount: "100.00", Currency: "MXN", ProviderRef: "ORD10"}
	seen := map[string]bool{}
	for _, amount := range []string{"30", "20", "20.00"} {
		o := Outcome{OrderRef: "SO-10", Kind: KindRefund, Status: StatusPartiallyRefunded, Amount: amount, Currency: "MXN", ProviderRef: "ORD10"}
		id := DedupeKey("mercadopago", o.DedupeEventID())
		if seen[id] {
			continue // the re-poll of the second refund
		}
		seen[id] = true
		if obs := ObserveOutcome(stored, o); obs.Decision != Apply {
			t.Fatalf("refund %s: %+v", amount, obs)
		}
		done := MustAmount(orZero(stored.AmountRefunded)).Add(MustAmount(amount))
		stored.AmountRefunded, stored.Status = done.String(), StatusPartiallyRefunded
	}
	if stored.AmountRefunded != "50.00" || len(seen) != 2 {
		t.Fatalf("amount_refunded %s after %d updates, want 50.00 after 2", stored.AmountRefunded, len(seen))
	}
}
