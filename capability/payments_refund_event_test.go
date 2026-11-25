package capability

import "testing"

func TestPaymentsRefundCreatedEventContract(t *testing.T) {
	e, ok := LookupEvent(CommercePaymentsRefundCreated)
	if !ok || e.EmittedBy != "commerce.payments" {
		t.Fatalf("event not registered: %+v", e)
	}
	if d, ok := ReservedEventDomain(CommercePaymentsRefundCreated); !ok || d != "commerce.payments" {
		t.Fatalf("domain = %q,%v", d, ok)
	}
	good := `{"provider":"stripe","emitter":"stripe","event":"commerce.payments.refund_created",
	  "refund_id":"re_1","order_id":"SO-1","status":"succeeded","amount":"50.00","currency":"MXN","refund":{"reason":"requested_by_customer"}}`
	if p := e.ValidatePayload([]byte(good)); len(p) != 0 {
		t.Fatalf("valid payload rejected: %v", p)
	}
	minimal := `{"provider":"mercadopago","refund_id":"9","status":"manual_required","amount":"1","currency":"ARS"}`
	if p := e.ValidatePayload([]byte(minimal)); len(p) != 0 {
		t.Fatalf("minimal payload rejected: %v", p)
	}
	for name, bad := range map[string]string{
		"no refund_id": `{"provider":"stripe","status":"succeeded","amount":"1","currency":"MXN"}`,
		"bad status":   `{"provider":"stripe","refund_id":"r","status":"done","amount":"1","currency":"MXN"}`,
		"float amount": `{"provider":"stripe","refund_id":"r","status":"succeeded","amount":1.5,"currency":"MXN"}`,
		"no currency":  `{"provider":"stripe","refund_id":"r","status":"succeeded","amount":"1"}`,
	} {
		if p := e.ValidatePayload([]byte(bad)); len(p) == 0 {
			t.Fatalf("%s accepted", name)
		}
	}
}
