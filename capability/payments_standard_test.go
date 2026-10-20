package capability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The payments standard only ADDS optional fields and contracts: the four
// original payment contracts keep their required inputs and outputs, so every
// provider and consumer built against them keeps working.
func TestPaymentsStandardRetrocompatible(t *testing.T) {
	want := map[string][2]string{
		CommercePaymentsSession: {"order_ref,amount,currency,return_url,idempotency_key", "session_id,status"},
		CommercePaymentsConfirm: {"payload", "order_ref,status,amount,currency,provider_ref"},
		CommercePaymentsStatus:  {"", "order_ref,status,amount,currency,provider_ref"},
		CommercePaymentsRefund:  {"provider_ref,amount,currency,idempotency_key", "refund_id,status,amount,currency"},
	}
	for key, w := range want {
		c := mustContract(t, key)
		if got := strings.Join(c.Input.Required(), ","); got != w[0] {
			t.Errorf("%s required input = %s, want %s", key, got, w[0])
		}
		if got := strings.Join(c.Output.Required(), ","); got != w[1] {
			t.Errorf("%s required output = %s, want %s", key, got, w[1])
		}
	}
	conf := mustContract(t, CommercePaymentsConfirm)
	// A pre-standard provider answer is still valid.
	if errs := conf.ValidateOutput([]byte(`{"order_ref":"O1","status":"paid","amount":"10.00","currency":"MXN","provider_ref":"1","kind":"payment","fee":"0.35","net_amount":"9.65"}`)); len(errs) != 0 {
		t.Fatalf("legacy confirm output rejected: %v", errs)
	}
	refund := mustContract(t, CommercePaymentsRefund)
	if errs := refund.ValidateOutput([]byte(`{"refund_id":"r","status":"manual_required","amount":"1","currency":"MXN","instructions":"cash"}`)); len(errs) != 0 {
		t.Fatalf("legacy refund output rejected: %v", errs)
	}
}

func TestPaymentsStandardContracts(t *testing.T) {
	cases := []struct {
		key                string
		pinned, idempotent bool
	}{
		{CommercePaymentsCancel, true, true},
		{CommercePaymentsCapabilities, true, false},
		{CommercePaymentsReconcile, true, false},
		{CommercePaymentsRefundStatus, true, false},
		{CommercePaymentsTerminalCreate, true, true},
		{CommercePaymentsTerminalDevices, true, false},
	}
	for _, c := range cases {
		ct := mustContract(t, c.key)
		if !ct.IsQuery() || ct.Fanout() || ct.PinRequired != c.pinned || ct.Idempotent != c.idempotent || ct.Input == nil {
			t.Errorf("%s: query=%v fanout=%v pinned=%v idempotent=%v", c.key, ct.IsQuery(), ct.Fanout(), ct.PinRequired, ct.Idempotent)
		}
		checkNoFloatMoney(t, c.key, ct.Input)
		checkNoFloatMoney(t, c.key, ct.Output)
		if !strings.HasPrefix(c.key, "commerce.payments.") {
			t.Errorf("%s outside the payments domain: its providers could not emit the neutral event", c.key)
		}
	}
	if !strings.HasPrefix(CommercePaymentsTerminalCreate, CommercePaymentsTerminalDomain+".") {
		t.Fatal("terminal domain")
	}
	// Terminal creation never selects: the device belongs to one provider.
	g := &Engine{Source: pinSource(Provider{Addon: "mercadopago", Default: true}), Invoke: func(context.Context, Call) (json.RawMessage, error) {
		t.Error("must not invoke")
		return nil, nil
	}}
	in := map[string]any{"device_id": "PAX", "order_ref": "SO-1", "amount": "100", "currency": "MXN", "idempotency_key": "k"}
	if _, err := g.Query(context.Background(), Query{Org: org, Capability: CommercePaymentsTerminalCreate, Input: in}); !errors.Is(err, ErrProviderRequired) {
		t.Fatalf("terminal create without provider: %v", err)
	}
	delete(in, "idempotency_key")
	if errs := mustContract(t, CommercePaymentsTerminalCreate).CheckInput(in); len(errs) != 1 {
		t.Fatalf("idempotency_key must be required: %v", errs)
	}
}

func TestPaymentsStandardOutputs(t *testing.T) {
	conf := mustContract(t, CommercePaymentsConfirm)
	terminal := `{"order_ref":"SO-1","status":"at_device","amount":"1499.00","currency":"MXN","provider_ref":"ORD01",
	  "channel":"terminal","device_id":"PAX_A910","session_id":"ORD01","provider_status":"at_terminal",
	  "card":{"type":"debit","brand":"debmaster","last4":"0042"}}`
	if errs := conf.ValidateOutput([]byte(terminal)); len(errs) != 0 {
		t.Fatalf("terminal status rejected: %v", errs)
	}
	for _, st := range []string{"at_device", "cancelling"} {
		raw := `{"order_ref":"O","status":"` + st + `","amount":"1","currency":"MXN","provider_ref":"1"}`
		if errs := conf.ValidateOutput([]byte(raw)); len(errs) != 0 {
			t.Errorf("status %s rejected: %v", st, errs)
		}
	}
	marketplace := `{"order_ref":"O","status":"paid","amount":"500.00","currency":"MXN","provider_ref":"1",
	  "seller":{"account_ref":"mp-user-77","seller_id":"v1","application_fee":"40.00"}}`
	if errs := conf.ValidateOutput([]byte(marketplace)); len(errs) != 0 {
		t.Fatalf("marketplace payment rejected: %v", errs)
	}
	for name, bad := range map[string]string{
		"channel":    `{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1","channel":"pos"}`,
		"card type":  `{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1","card":{"type":"amex"}}`,
		"pan":        `{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1","card":{"number":"4111111111111111"}}`,
		"seller fee": `{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1","seller":{"account_ref":"a","application_fee":0.4}}`,
		"no seller":  `{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1","seller":{"seller_id":"v"}}`,
	} {
		if errs := conf.ValidateOutput([]byte(bad)); len(errs) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
	rec := mustContract(t, CommercePaymentsReconcile)
	if errs := rec.ValidateOutput([]byte(`{"payments":[{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1"}],"next_cursor":"c2"}`)); len(errs) != 0 {
		t.Fatalf("reconcile page rejected: %v", errs)
	}
	if errs := rec.ValidateOutput([]byte(`{"payments":[{"order_ref":"O","status":"approved","amount":"1","currency":"MXN","provider_ref":"1"}]}`)); len(errs) == 0 {
		t.Fatal("reconcile items must be normalized status outputs")
	}
	tc := mustContract(t, CommercePaymentsTerminalCreate)
	if errs := tc.ValidateOutput([]byte(`{"provider_ref":"ORD01","status":"at_device","expires_at":"2026-10-18T10:10:00Z","installments":1,"message":"installments not accepted, retried without"}`)); len(errs) != 0 {
		t.Fatalf("terminal create output rejected: %v", errs)
	}
	if errs := tc.ValidateOutput([]byte(`{"provider_ref":"ORD01","status":"paid"}`)); len(errs) == 0 {
		t.Fatal("terminal create cannot answer paid: the device has not been used yet")
	}
	cancel := mustContract(t, CommercePaymentsCancel)
	if errs := cancel.ValidateOutput([]byte(`{"status":"cancelling","payment_status":"cancelling"}`)); len(errs) != 0 {
		t.Fatalf("async cancel rejected: %v", errs)
	}
	caps := mustContract(t, CommercePaymentsCapabilities)
	if errs := caps.ValidateOutput([]byte(`{"channels":["terminal","online","qr"],"min_amount":"5.00","partial_refunds":true,"cancel_is_async":true,"terminal_expires_seconds":600}`)); len(errs) != 0 {
		t.Fatalf("capabilities rejected: %v", errs)
	}
	devices := mustContract(t, CommercePaymentsTerminalDevices)
	if errs := devices.ValidateOutput([]byte(`{"devices":[{"device_id":"PAX","mode":"PDV","status":"online"}]}`)); len(errs) != 0 {
		t.Fatalf("devices rejected: %v", errs)
	}
}

// Every payments provider, including a terminal-only one, may emit the
// neutral event; a sales channel may not.
func TestPaymentsConfirmedEmitters(t *testing.T) {
	d, ok := ReservedEventDomain(CommercePaymentsConfirmed)
	if !ok || d != "commerce.payments" {
		t.Fatalf("domain %q", d)
	}
	if !ProvidesDomain([]string{CommercePaymentsTerminalCreate}, d) {
		t.Fatal("terminal provider must be able to emit")
	}
	if ProvidesDomain([]string{"commerce.shipping.rates"}, d) || ProvidesDomain(nil, d) {
		t.Fatal("non-provider emits")
	}
	e, _ := LookupEvent(CommercePaymentsConfirmed)
	// mercadopago 0.8.0 already publishes organization_id and occurred_at.
	mp := `{"provider":"mercadopago","organization_id":"o","occurred_at":"2026-10-18T10:00:00Z","source":"webhook",
	  "result":{"order_ref":"SO-1","status":"paid","amount":"1","currency":"MXN","provider_ref":"1"}}`
	if p := e.ValidatePayload([]byte(mp)); len(p) != 0 {
		t.Fatalf("mercadopago payload rejected: %v", p)
	}
}
