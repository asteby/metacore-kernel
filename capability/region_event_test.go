package capability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRegionPostalLocateContract(t *testing.T) {
	c := mustContract(t, RegionPostalLocate)
	if !c.IsQuery() || c.Fanout() || c.Timeout != RegionPostalTimeout || c.Idempotent || c.PinRequired {
		t.Fatalf("unexpected contract shape: %+v", c)
	}
	if strings.Join(c.Required, ",") != "postal_code" {
		t.Fatalf("required = %v", c.Required)
	}
	for _, in := range []string{"06600", "64000"} {
		if p := c.CheckInput(map[string]any{"postal_code": in}); len(p) != 0 {
			t.Fatalf("%q rejected: %v", in, p)
		}
	}
	for _, in := range []any{"6600", "066000", "0660A", " 06600", 6600} {
		if p := c.CheckInput(map[string]any{"postal_code": in}); len(p) == 0 {
			t.Fatalf("%v accepted, want 5 digits", in)
		}
	}
	if p := c.CheckInput(map[string]any{}); len(p) != 1 || !strings.Contains(p[0], "required") {
		t.Fatalf("missing postal_code: %v", p)
	}

	ok := `{"lat":19.4326,"lng":-99.1332,"city":"Ciudad de México","state":"CDMX","precision":"postal_centroid"}`
	if p := c.ValidateOutput([]byte(ok)); len(p) != 0 {
		t.Fatalf("valid output rejected: %v", p)
	}
	if p := c.ValidateOutput([]byte(`{"lat":19.4,"lng":-99.1,"state":"CDMX","precision":"postal_centroid"}`)); len(p) != 0 {
		t.Fatalf("city must be optional: %v", p)
	}
	bad := map[string]string{
		"rooftop precision": `{"lat":1,"lng":1,"state":"X","precision":"rooftop"}`,
		"string lat":        `{"lat":"19.4","lng":1,"state":"X","precision":"postal_centroid"}`,
		"unknown field":     `{"lat":1,"lng":1,"state":"X","precision":"postal_centroid","colonia":"Roma"}`,
		"missing state":     `{"lat":1,"lng":1,"precision":"postal_centroid"}`,
	}
	for name, out := range bad {
		if p := c.ValidateOutput([]byte(out)); len(p) == 0 {
			t.Fatalf("%s: output accepted, want closed schema rejection", name)
		}
	}
}

func TestCodedErrorsFollowTheContract(t *testing.T) {
	c := mustContract(t, RegionPostalLocate)
	run := func(err error) Result {
		res, qerr := Executor{}.Run(context.Background(), c, org, "mx", []Provider{{Addon: "region_mx"}},
			map[string]any{"postal_code": "99999"},
			func(context.Context, Call) (json.RawMessage, error) { return nil, err })
		if qerr != nil {
			t.Fatalf("run: %v", qerr)
		}
		return res.Results[0]
	}
	if got := run(NewCodedError(ErrCodeNotFound, "99999 not in SEPOMEX")).ErrorCode(); got != "not_found" {
		t.Fatalf("declared code = %q, want not_found", got)
	}
	r := run(NewCodedError(ErrCodeInvalidInput, "reserved range"))
	if r.ErrorCode() != "invalid_input" || !strings.Contains(r.Err.Error(), "reserved range") {
		t.Fatalf("invalid_input lost: %q %v", r.ErrorCode(), r.Err)
	}
	r = run(NewCodedError("quota_exceeded", "too many lookups"))
	if r.ErrorCode() != "provider_error" || !strings.Contains(r.Err.Error(), "too many lookups") {
		t.Fatalf("undeclared code must degrade to provider_error keeping the message: %q %v", r.ErrorCode(), r.Err)
	}
	if got := run(errors.New("boom")).ErrorCode(); got != "provider_error" {
		t.Fatalf("plain error = %q", got)
	}
	env := QueryResult{Capability: c.Key, Results: []Result{run(NewCodedError(ErrCodeNotFound, ""))}}.Envelope()
	if len(env.Errors) != 1 || env.Errors[0].Code != "not_found" {
		t.Fatalf("envelope errors = %+v", env.Errors)
	}
}

func TestRegisterRejectsBadErrorsAndPatterns(t *testing.T) {
	out := schema(req("x", TypeString, ""))
	cases := map[string]Contract{
		"reserved code":    {Key: "acme.reserved.code", Kind: KindQuery, Output: out, Errors: []string{"timeout"}},
		"malformed code":   {Key: "acme.malformed.code", Kind: KindQuery, Output: out, Errors: []string{"Not-Found"}},
		"bad pattern":      {Key: "acme.bad.pattern", Kind: KindQuery, Output: out, Input: schema(pattern(req("a", TypeString, ""), `([`))},
		"nested bad regex": {Key: "acme.nested.pattern", Kind: KindQuery, Output: schema(obj(req("o", TypeObject, ""), schema(pattern(req("a", TypeString, ""), `(`))))},
	}
	for name, c := range cases {
		if err := Register(c); err == nil {
			delete(Contracts, c.Key)
			t.Fatalf("%s: Register accepted %+v", name, c)
		}
	}
}

func TestPaymentsConfirmedEventContract(t *testing.T) {
	e, ok := LookupEvent(CommercePaymentsConfirmed)
	if !ok || e.EmittedBy != "commerce.payments" {
		t.Fatalf("event not registered: %+v", e)
	}
	if names := EventNames(); len(names) == 0 || names[1] != CommercePaymentsConfirmed {
		t.Fatalf("EventNames = %v", names)
	}
	good := `{"provider":"stripe","emitter":"stripe","event":"commerce.payments.confirmed",
	  "result":{"order_ref":"SO-1","status":"paid","amount":"199.90","currency":"MXN","provider_ref":"pi_1","kind":"payment"}}`
	if p := e.ValidatePayload([]byte(good)); len(p) != 0 {
		t.Fatalf("valid payload rejected (host-stamped keys must be ignored): %v", p)
	}
	refund := `{"provider":"mercadopago","result":{"order_ref":"SO-1","status":"refunded","amount":"50","currency":"MXN","provider_ref":"123","kind":"refund"}}`
	if p := e.ValidatePayload([]byte(refund)); len(p) != 0 {
		t.Fatalf("refund rejected: %v", p)
	}
	for name, bad := range map[string]string{
		"no result":  `{"provider":"stripe"}`,
		"float":      `{"provider":"stripe","result":{"order_ref":"SO-1","status":"paid","amount":199.9,"currency":"MXN","provider_ref":"pi"}}`,
		"bad kind":   `{"provider":"stripe","result":{"order_ref":"SO-1","status":"paid","amount":"1","currency":"MXN","provider_ref":"pi","kind":"chargeback"}}`,
		"not object": `[1]`,
	} {
		if p := e.ValidatePayload([]byte(bad)); len(p) == 0 {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestReservedEventDomainAndProviders(t *testing.T) {
	for ev, want := range map[string]string{
		"commerce.payments.confirmed":        "commerce.payments",
		"commerce.payments.anything":         "commerce.payments",
		"commerce.shipping.tracking_updated": "commerce.shipping",
		"commerce.shipping.label_ready":      "commerce.shipping",
		"commerce.fulfillment.updated":       "commerce.fulfillment",
	} {
		if d, ok := ReservedEventDomain(ev); !ok || d != want {
			t.Fatalf("%s: domain = %q,%v want %q", ev, d, ok, want)
		}
	}
	for _, ev := range []string{"commerce.paymentsx.y", "commerce.shippingx.tracking_updated", "stripe.payment_updated", "shipping.tracking_updated", "emissary.shipment_status_changed", "commerce.tax.calculated"} {
		if d, ok := ReservedEventDomain(ev); ok {
			t.Fatalf("%s must not be reserved, got %q", ev, d)
		}
	}
	if !ProvidesDomain([]string{"commerce.payments.confirm"}, "commerce.payments") {
		t.Fatal("confirm provider must provide the domain")
	}
	if ProvidesDomain([]string{"commerce.paymentsx.foo", "commerce.shipping.rates"}, "commerce.payments") {
		t.Fatal("prefix without dot boundary must not match")
	}
}

func TestRegisterEventValidation(t *testing.T) {
	p := schema(req("x", TypeString, ""))
	for name, e := range map[string]EventContract{
		"outside domain": {Name: "acme.thing.done", EmittedBy: "commerce.payments", Payload: p},
		"no payload":     {Name: "acme.thing.done", EmittedBy: "acme.thing"},
		"bad name":       {Name: "Acme", EmittedBy: "acme.thing", Payload: p},
		"duplicate":      {Name: CommercePaymentsConfirmed, EmittedBy: "commerce.payments", Payload: p},
	} {
		if err := RegisterEvent(e); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
