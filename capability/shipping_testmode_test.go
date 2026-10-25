package capability

import "testing"

// test_mode is the official field of the rate / label / fulfillment outputs and
// travels next to the vendor extension x_environment skydropx already emits.
func TestShippingOutputsAcceptTestModeAndXEnvironment(t *testing.T) {
	rates := mustContract(t, CommerceShippingRates)
	rate := `{"rates":[{"id":"r1","carrier":"fedex","service":"std","cost":"99.00","currency":"MXN","test_mode":true,"x_environment":"sandbox"}]}`
	if p := rates.ValidateOutput([]byte(rate)); len(p) != 0 {
		t.Fatalf("rates with test_mode rejected: %v", p)
	}
	if p := rates.ValidateOutput([]byte(`{"rates":[{"id":"r1","carrier":"fedex","service":"std","cost":"99.00","currency":"MXN","test_mode":"yes"}]}`)); len(p) == 0 {
		t.Fatal("non-boolean test_mode accepted")
	}
	if p := rates.ValidateOutput([]byte(`{"rates":[{"id":"r1","carrier":"fedex","service":"std","cost":"99.00","currency":"MXN"}]}`)); len(p) != 0 {
		t.Fatalf("absent test_mode must stay valid: %v", p)
	}
	label := mustContract(t, CommerceShippingLabel)
	if p := label.ValidateOutput([]byte(`{"label_status":"created","carrier":"fedex","test_mode":false,"x_environment":"production"}`)); len(p) != 0 {
		t.Fatalf("label with test_mode rejected: %v", p)
	}
	if p := label.ValidateOutput([]byte(`{"label_status":"created","carrier":"fedex","test_mode":1}`)); len(p) == 0 {
		t.Fatal("non-boolean label test_mode accepted")
	}
	ful := mustContract(t, CommerceFulfillmentCreate)
	if p := ful.ValidateOutput([]byte(`{"fulfillment_id":"F1","status":"accepted","test_mode":true}`)); len(p) != 0 {
		t.Fatalf("fulfillment with test_mode rejected: %v", p)
	}
}

func TestShipmentsUpdatedEventContract(t *testing.T) {
	e, ok := LookupEvent(CommerceShipmentsUpdated)
	if !ok || e.EmittedBy != "commerce.shipments" || CommerceShipmentsUpdated != "commerce.shipments.updated" {
		t.Fatalf("event not registered: %+v", e)
	}
	if d, ok := ReservedEventDomain(CommerceShipmentsUpdated); !ok || d != "commerce.shipments" {
		t.Fatalf("domain not reserved: %q %v", d, ok)
	}
	good := `{"emitter":"shipments","event":"commerce.shipments.updated","shipment_id":"shp_1","event_id":"shp_1|delivered",
	  "status":"delivered","previous_status":"out_for_delivery","occurred_at":"2026-10-18T10:00:00Z","order_ref":"SO-1","mode":"carrier",
	  "provider":"skydropx","carrier":"fedex","tracking_number":"7489","promised_by":"2026-10-20","source":"webhook","test_mode":true}`
	if p := e.ValidatePayload([]byte(good)); len(p) != 0 {
		t.Fatalf("valid payload rejected: %v", p)
	}
	for _, st := range ShipmentStatuses {
		raw := `{"shipment_id":"s","event_id":"e","status":"` + st + `","occurred_at":"t"}`
		if p := e.ValidatePayload([]byte(raw)); len(p) != 0 {
			t.Fatalf("status %s rejected: %v", st, p)
		}
	}
	for name, bad := range map[string]string{
		"no shipment_id": `{"event_id":"e","status":"delivered","occurred_at":"t"}`,
		"no event_id":    `{"shipment_id":"s","status":"delivered","occurred_at":"t"}`,
		"unknown status": `{"shipment_id":"s","event_id":"e","status":"lost_forever","occurred_at":"t"}`,
		"bad mode":       `{"shipment_id":"s","event_id":"e","status":"delivered","occurred_at":"t","mode":"drone"}`,
		"bad test_mode":  `{"shipment_id":"s","event_id":"e","status":"delivered","occurred_at":"t","test_mode":"si"}`,
	} {
		if p := e.ValidatePayload([]byte(bad)); len(p) == 0 {
			t.Fatalf("%s accepted", name)
		}
	}
}
