package capability

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOutputValidation(t *testing.T) {
	c := mustContract(t, CommerceShippingRates)
	cases := map[string]struct {
		out  string
		want []string // substrings; empty = valid
	}{
		"valid":            {`{"rates":[{"id":"a","carrier":"fedex","service":"Exp","cost":"150.50","currency":"MXN","eta_days_min":1,"x_raw":{}}],"x_debug":true}`, nil},
		"float money":      {`{"rates":[{"id":"a","carrier":"c","service":"s","cost":150.5,"currency":"MXN"}]}`, []string{"rates[0].cost: want decimal string"}},
		"bad currency":     {`{"rates":[{"id":"a","carrier":"c","service":"s","cost":"1","currency":"mxn"}]}`, []string{"rates[0].currency"}},
		"missing required": {`{"rates":[{"carrier":"c","service":"s","cost":"1","currency":"MXN"}]}`, []string{"rates[0].id: required"}},
		"fractional eta":   {`{"rates":[{"id":"a","carrier":"c","service":"s","cost":"1","currency":"MXN","eta_days_max":1.5}]}`, []string{"eta_days_max: want integer"}},
		"unknown field":    {`{"rates":[],"carrier_quotes":[]}`, []string{"carrier_quotes: unknown field"}},
		"not an object":    {`[1,2]`, []string{"not a JSON object"}},
		"null":             {`null`, []string{"null"}},
		"no rates":         {`{}`, []string{"rates: required"}},
	}
	for name, tc := range cases {
		got := c.ValidateOutput([]byte(tc.out))
		if len(tc.want) == 0 && len(got) != 0 {
			t.Errorf("%s: unexpected errors %v", name, got)
		}
		for _, w := range tc.want {
			if !strings.Contains(strings.Join(got, "; "), w) {
				t.Errorf("%s: want %q in %v", name, w, got)
			}
		}
	}

	conf := mustContract(t, CommercePaymentsConfirm)
	if errs := conf.ValidateOutput([]byte(`{"order_ref":"O1","status":"paid","amount":"10.00","currency":"MXN","provider_ref":"123"}`)); len(errs) != 0 {
		t.Fatalf("confirm valid: %v", errs)
	}
	if errs := conf.ValidateOutput([]byte(`{"order_ref":"O1","status":"approved","amount":"10.00","currency":"MXN","provider_ref":"123"}`)); len(errs) != 1 || !strings.Contains(errs[0], "not one of") {
		t.Fatalf("provider status must be normalized: %v", errs)
	}
	track := mustContract(t, CommerceShippingTrack)
	if errs := track.ValidateOutput([]byte(`{"status":"in_transit","events":[{"at":"2026-10-16T10:00:00Z","status":"en_ruta"}]}`)); len(errs) != 1 || !strings.Contains(errs[0], "events[0].status") {
		t.Fatalf("tracking statuses normalized: %v", errs)
	}
}

func TestCheckInputIsOpenButTyped(t *testing.T) {
	c := mustContract(t, CommerceTaxCalculate)
	in := map[string]any{
		"currency":   "MXN",
		"lines":      []any{map[string]any{"id": "1", "quantity": float64(2), "unit_price": "100.00", "x": 1}},
		"extra_hint": "passes through",
	}
	if errs := c.CheckInput(in); len(errs) != 0 {
		t.Fatalf("valid input rejected: %v", errs)
	}
	in["lines"] = []any{map[string]any{"id": "1", "quantity": 1.5, "unit_price": 100}}
	errs := c.CheckInput(in)
	if len(errs) != 2 {
		t.Fatalf("want quantity+unit_price errors, got %v", errs)
	}
}

func TestDecimalsAndCountries(t *testing.T) {
	for _, s := range []string{"0", "12", "199.90", "-0.5", "0.0825"} {
		if !ValidDecimal(s) {
			t.Errorf("%q must be a decimal", s)
		}
	}
	for _, s := range []string{"", "1.", ".5", "1e3", "01", "1,000.00", " 1"} {
		if ValidDecimal(s) {
			t.Errorf("%q must not be a decimal", s)
		}
	}
	if !ValidCountry("MX") || ValidCountry("mx") || ValidCountry("MEX") {
		t.Fatal("countries are upper-case alpha-2")
	}
}

func TestCommerceContractRegistry(t *testing.T) {
	want := map[string]Cardinality{
		CommercePaymentsSession: One, CommercePaymentsConfirm: One, CommercePaymentsRefund: One, CommercePaymentsStatus: One,
		CommerceShippingCancel: One,
		CommerceTaxCalculate:   One, CommerceTaxCommit: One, CommerceTaxReverse: One,
		CommerceShippingRates: All, CommerceShippingLabel: One, CommerceShippingTrack: One,
		CommerceFulfillmentCreate: One, CommerceFulfillmentCancel: One,
		CommerceSearchQuery: One, CommerceDiscountsEvaluate: All,
		FiscalInvoiceCreate: One, FiscalInvoiceCancel: One,
	}
	for key, card := range want {
		c := mustContract(t, key)
		if !c.IsQuery() || c.Output == nil || c.Input == nil || c.Cardinality != card || c.Key != key {
			t.Errorf("%s: kind=%s card=%s output=%v", key, c.Kind, c.Cardinality, c.Output != nil)
		}
		// Required/Optional are derived from the typed input (the mapping
		// validators read them).
		if strings.Join(c.Required, ",") != strings.Join(c.Input.Required(), ",") || len(c.Fields()) != len(c.Input.Fields) {
			t.Errorf("%s: name lists not derived from Input", key)
		}
		// Every money field is a decimal next to a currency, never a number.
		checkNoFloatMoney(t, key, c.Input)
		checkNoFloatMoney(t, key, c.Output)
	}
	if !mustContract(t, CommerceShippingRates).Has("parcels") || mustContract(t, CommerceShippingRates).Timeout != ShippingRatesTimeout {
		t.Fatal("rates contract shape")
	}
	if c := mustContract(t, MessagingWhatsAppSend); c.IsQuery() || c.Fanout() {
		t.Fatal("whatsapp send stays an action")
	}
	keys := Keys()
	if len(keys) < len(want)+1 || keys[0] > keys[len(keys)-1] {
		t.Fatalf("keys = %v", keys)
	}
}

func checkNoFloatMoney(t *testing.T, key string, s *Schema) {
	t.Helper()
	if s == nil {
		return
	}
	for _, f := range s.Fields {
		switch f.Name {
		case "amount", "cost", "unit_price", "total", "subtotal", "tax", "total_tax", "declared_value", "discount", "fee", "net_amount", "shipping_amount", "shipping_tax", "cod_amount", "amount_refunded", "refund_amount", "subtotal_amount", "shipping":
			if f.Type != TypeDecimal {
				t.Errorf("%s: money field %s is %q, want decimal", key, f.Name, f.Type)
			}
		}
		if f.Of != nil {
			checkNoFloatMoney(t, key, f.Of)
		}
	}
}

func TestRegisterRejectsMalformedContracts(t *testing.T) {
	cases := map[string]Contract{
		"bad key":        {Key: "Payments"},
		"duplicate":      {Key: CommercePaymentsSession, Kind: KindQuery, Output: &Schema{}},
		"query no out":   {Key: "acme.x.query", Kind: KindQuery},
		"action fan-out": {Key: "acme.x.act", Cardinality: All},
		"unknown kind":   {Key: "acme.x.kind", Kind: "stream"},
		"bad card":       {Key: "acme.x.card", Kind: KindQuery, Output: &Schema{}, Cardinality: "some"},
	}
	for name, c := range cases {
		if err := Register(c); err == nil {
			t.Errorf("%s: accepted", name)
			delete(Contracts, c.Key)
		}
	}
	ok := Contract{Key: "acme.loyalty.points", Kind: KindQuery, Timeout: time.Second,
		Input:  schema(req("customer_id", TypeString, "")),
		Output: schema(req("points", TypeInteger, ""))}
	if err := Register(ok); err != nil {
		t.Fatal(err)
	}
	defer delete(Contracts, ok.Key)
	got, _ := Lookup(ok.Key)
	if strings.Join(got.Required, ",") != "customer_id" {
		t.Fatalf("required not derived: %+v", got)
	}
	if errs := got.ValidateOutput(json.RawMessage(`{"points":10}`)); len(errs) != 0 {
		t.Fatal(errs)
	}
}
