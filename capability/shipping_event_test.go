package capability

import (
	"reflect"
	"strings"
	"testing"
)

func TestEventNamesSorted(t *testing.T) {
	want := []string{CommerceFulfillmentUpdated, CommercePaymentsConfirmed, CommerceShippingTrackingUpdated}
	got := EventNames()
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("EventNames = %v, missing %s", got, w)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("EventNames not sorted: %v", got)
		}
	}
}

func TestTrackingUpdatedEventContract(t *testing.T) {
	e, ok := LookupEvent(CommerceShippingTrackingUpdated)
	if !ok || e.EmittedBy != "commerce.shipping" {
		t.Fatalf("event not registered: %+v", e)
	}
	good := `{"provider":"skydropx","emitter":"skydropx","event":"commerce.shipping.tracking_updated",
	  "event_id":"shp_1|DELIVERED","status":"delivered","occurred_at":"2026-10-18T10:00:00Z",
	  "order_ref":"SO-1","shipment_id":"shp_1","carrier":"fedex","tracking_number":"7489","tracking_url":"https://t/7489",
	  "provider_status":"DELIVERED","location":"Monterrey","x_raw_code":"41"}`
	if p := e.ValidatePayload([]byte(good)); len(p) != 0 {
		t.Fatalf("valid payload rejected: %v", p)
	}
	minimal := `{"provider":"envia","event_id":"k","status":"created","occurred_at":"2026-10-18T10:00:00Z"}`
	if p := e.ValidatePayload([]byte(minimal)); len(p) != 0 {
		t.Fatalf("minimal payload rejected (order_ref and tracking are optional: a label can be pending): %v", p)
	}
	for name, bad := range map[string]string{
		"no event_id":    `{"provider":"envia","status":"created","occurred_at":"2026-10-18T10:00:00Z"}`,
		"no provider":    `{"event_id":"k","status":"created","occurred_at":"2026-10-18T10:00:00Z"}`,
		"unknown status": `{"provider":"envia","event_id":"k","status":"lost","occurred_at":"2026-10-18T10:00:00Z"}`,
		"legacy key":     `{"provider":"envia","event_id":"k","status":"created","occurred_at":"2026-10-18T10:00:00Z","tracking":"7489"}`,
		"bool as string": `{"provider":"envia","event_id":"k","status":"created","occurred_at":"x","label_ready":"yes"}`,
	} {
		if p := e.ValidatePayload([]byte(bad)); len(p) == 0 {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestFulfillmentUpdatedEventContract(t *testing.T) {
	e, ok := LookupEvent(CommerceFulfillmentUpdated)
	if !ok || e.EmittedBy != "commerce.fulfillment" {
		t.Fatalf("event not registered: %+v", e)
	}
	good := `{"provider":"emissary","emitter":"emissary","event_id":"F-9:in_transit","fulfillment_id":"F-9","order_ref":"SO-1",
	  "status":"in_transit","previous_status":"picked_up","occurred_at":"2026-10-18T10:00:00Z","carrier":"estafeta",
	  "tracking_number":"123","provider_status":"4","test_mode":true}`
	if p := e.ValidatePayload([]byte(good)); len(p) != 0 {
		t.Fatalf("valid payload rejected: %v", p)
	}
	for _, st := range []string{"accepted", "on_hold", "rejected", "delivered", "cancelled"} {
		raw := `{"provider":"x","event_id":"e","fulfillment_id":"f","order_ref":"o","status":"` + st + `","occurred_at":"t"}`
		if p := e.ValidatePayload([]byte(raw)); len(p) != 0 {
			t.Fatalf("status %s rejected: %v", st, p)
		}
	}
	for name, bad := range map[string]string{
		"no fulfillment": `{"provider":"x","event_id":"e","order_ref":"o","status":"accepted","occurred_at":"t"}`,
		"no order_ref":   `{"provider":"x","event_id":"e","fulfillment_id":"f","status":"accepted","occurred_at":"t"}`,
		"bad previous":   `{"provider":"x","event_id":"e","fulfillment_id":"f","order_ref":"o","status":"accepted","previous_status":"shipped","occurred_at":"t"}`,
	} {
		if p := e.ValidatePayload([]byte(bad)); len(p) == 0 {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The fulfillment vocabulary is the create output followed by the parcel
// lifecycle, and appending to it must not alias TrackingStatuses.
func TestFulfillmentStatusesVocabulary(t *testing.T) {
	var create []string
	for _, f := range Contracts[CommerceFulfillmentCreate].Output.Fields {
		if f.Name == "status" {
			create = f.Enum
		}
	}
	if !reflect.DeepEqual(FulfillmentStatuses, append(append([]string{}, create...), TrackingStatuses...)) {
		t.Fatalf("FulfillmentStatuses = %v, want create %v + tracking %v", FulfillmentStatuses, create, TrackingStatuses)
	}
	if strings.Join(TrackingStatuses, ",") != "created,picked_up,in_transit,out_for_delivery,delivered,exception,returned,cancelled" {
		t.Fatalf("TrackingStatuses changed: %v", TrackingStatuses)
	}
}

// Every carrier and 3PL provides a capability of the domain it emits in; a
// storefront addon does not.
func TestShippingDomainsProviders(t *testing.T) {
	cases := []struct {
		provides []string
		event    string
		ok       bool
	}{
		{[]string{"commerce.shipping.rates", "commerce.shipping.label"}, CommerceShippingTrackingUpdated, true},
		{[]string{"commerce.fulfillment.create"}, CommerceFulfillmentUpdated, true},
		{[]string{"commerce.fulfillment.create", "commerce.shipping.rates"}, CommerceShippingTrackingUpdated, true},
		{[]string{"commerce.fulfillment.create"}, CommerceShippingTrackingUpdated, false},
		{[]string{"commerce.payments.session"}, CommerceFulfillmentUpdated, false},
		{nil, CommerceShippingTrackingUpdated, false},
	}
	for _, c := range cases {
		d, reserved := ReservedEventDomain(c.event)
		if !reserved {
			t.Fatalf("%s not reserved", c.event)
		}
		if got := ProvidesDomain(c.provides, d); got != c.ok {
			t.Fatalf("%v emitting %s: %v, want %v", c.provides, c.event, got, c.ok)
		}
	}
}

// eta (both events) and location (fulfillment) are additive: the previous
// payloads keep validating and the new fields are typed.
func TestTrackingEtaAndFulfillmentLocationAreAdditive(t *testing.T) {
	tr, _ := LookupEvent(CommerceShippingTrackingUpdated)
	ff, _ := LookupEvent(CommerceFulfillmentUpdated)
	for name, c := range map[string]struct {
		e   EventContract
		raw string
		ok  bool
	}{
		"tracking with eta":                     {tr, `{"provider":"skydropx","event_id":"e","status":"in_transit","occurred_at":"2026-10-29T10:00:00Z","location":"Toluca","eta":"2026-10-31T00:00:00Z"}`, true},
		"tracking without eta (previous shape)": {tr, `{"provider":"skydropx","event_id":"e","status":"in_transit","occurred_at":"2026-10-29T10:00:00Z"}`, true},
		"tracking eta wrong type":               {tr, `{"provider":"skydropx","event_id":"e","status":"in_transit","occurred_at":"x","eta":5}`, false},
		"fulfillment with location and eta":     {ff, `{"provider":"emissary","event_id":"e","fulfillment_id":"f","order_ref":"o","status":"in_transit","occurred_at":"2026-10-29T10:00:00Z","location":"Monterrey","eta":"2026-10-31T00:00:00Z"}`, true},
		"fulfillment previous shape":            {ff, `{"provider":"emissary","event_id":"e","fulfillment_id":"f","order_ref":"o","status":"accepted","occurred_at":"2026-10-29T10:00:00Z"}`, true},
		"fulfillment location wrong type":       {ff, `{"provider":"emissary","event_id":"e","fulfillment_id":"f","order_ref":"o","status":"accepted","occurred_at":"x","location":{"city":"x"}}`, false},
	} {
		got := len(c.e.ValidatePayload([]byte(c.raw))) == 0
		if got != c.ok {
			t.Errorf("%s: valid=%v, want %v", name, got, c.ok)
		}
	}
}
