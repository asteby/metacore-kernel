package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const hash64 = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// rateWith builds one rates[] element from the legacy shape plus extra JSON
// members (without braces).
func rateWith(extra string) string {
	base := `{"id":"shipping_flat:flat:5f1c","carrier":"flat","service":"Envío estándar","cost":"0.00","currency":"MXN"`
	if extra != "" {
		base += "," + extra
	}
	return base + "}"
}

func rateOut(extra string) string { return `{"rates":[` + rateWith(extra) + `]}` }

// The exact shapes a provider built BEFORE this change answers and receives.
func TestShippingLegacyShapesStillValid(t *testing.T) {
	rates := mustContract(t, CommerceShippingRates)
	label := mustContract(t, CommerceShippingLabel)

	if errs := rates.CheckInput(ratesInput()); len(errs) != 0 {
		t.Fatalf("legacy rates input: %v", errs)
	}
	legacyOut := `{"rates":[{"id":"r1","carrier":"fedex","service":"Express","cost":"159.00","currency":"MXN",
	  "service_code":"EXP","eta_days_min":1,"eta_days_max":2,"pickup":false,"delivery_type":"door","taxes_included":true,"expires_at":"2026-10-21T10:00:00Z"}]}`
	if errs := rates.ValidateOutput([]byte(legacyOut)); len(errs) != 0 {
		t.Fatalf("legacy rates output: %v", errs)
	}
	legacyLabelIn := map[string]any{
		"rate_id": "r1", "order_ref": "SO-1", "idempotency_key": "shipment:1:label:1",
		"parcels": []any{map[string]any{"weight_kg": "1.2"}}, "declared_value": "749.00", "currency": "MXN",
		"label_format": "pdf", "label_size": "4x6", "cod_amount": "0",
		"items": []any{map[string]any{"sku": "A", "quantity": 1}},
	}
	if errs := label.CheckInput(legacyLabelIn); len(errs) != 0 {
		t.Fatalf("legacy label input: %v", errs)
	}
	legacyLabelOut := `{"label_status":"created","carrier":"fedex","tracking_number":"7489","label_url":"https://l/1","shipment_id":"s1",
	  "label_format":"pdf","tracking_url":"https://t/7489","cost":"120.00","currency":"MXN","status":"created"}`
	if errs := label.ValidateOutput([]byte(legacyLabelOut)); len(errs) != 0 {
		t.Fatalf("legacy label output: %v", errs)
	}
}

// Every field of the previous schemas is still there, same type and
// requiredness; the new ones are all optional.
func TestShippingEvolutionIsAdditiveOnly(t *testing.T) {
	rates := mustContract(t, CommerceShippingRates)
	label := mustContract(t, CommerceShippingLabel)

	if got := strings.Join(rates.Input.Required(), ","); got != "origin,destination,parcels,currency" {
		t.Errorf("rates input required = %s", got)
	}
	if got := strings.Join(label.Input.Required(), ","); got != "rate_id,order_ref,idempotency_key" {
		t.Errorf("label input required = %s", got)
	}
	if got := strings.Join(label.Output.Required(), ","); got != "label_status,carrier" {
		t.Errorf("label output required = %s", got)
	}
	ratesOutF, _ := rates.Output.Field("rates")
	if got := strings.Join(ratesOutF.Of.Required(), ","); got != "id,carrier,service,cost,currency" {
		t.Errorf("rates[] required = %s", got)
	}

	frozen := []struct {
		s     *Schema
		name  string
		field string
		typ   FieldType
	}{
		{rates.Input, "rates.in", "origin", TypeObject}, {rates.Input, "rates.in", "destination", TypeObject},
		{rates.Input, "rates.in", "parcels", TypeArray}, {rates.Input, "rates.in", "currency", TypeCurrency},
		{rates.Input, "rates.in", "declared_value", TypeDecimal}, {rates.Input, "rates.in", "cod_amount", TypeDecimal},
		{rates.Input, "rates.in", "items", TypeArray}, {rates.Input, "rates.in", "order_ref", TypeString},
		{ratesOutF.Of, "rates[]", "id", TypeString}, {ratesOutF.Of, "rates[]", "carrier", TypeString},
		{ratesOutF.Of, "rates[]", "service", TypeString}, {ratesOutF.Of, "rates[]", "cost", TypeDecimal},
		{ratesOutF.Of, "rates[]", "currency", TypeCurrency}, {ratesOutF.Of, "rates[]", "service_code", TypeString},
		{ratesOutF.Of, "rates[]", "eta_days_min", TypeInteger}, {ratesOutF.Of, "rates[]", "eta_days_max", TypeInteger},
		{ratesOutF.Of, "rates[]", "pickup", TypeBoolean}, {ratesOutF.Of, "rates[]", "delivery_type", TypeString},
		{ratesOutF.Of, "rates[]", "taxes_included", TypeBoolean}, {ratesOutF.Of, "rates[]", "expires_at", TypeString},
		{label.Input, "label.in", "rate_id", TypeString}, {label.Input, "label.in", "order_ref", TypeString},
		{label.Input, "label.in", "origin", TypeObject}, {label.Input, "label.in", "destination", TypeObject},
		{label.Input, "label.in", "parcels", TypeArray}, {label.Input, "label.in", "items", TypeArray},
		{label.Input, "label.in", "declared_value", TypeDecimal}, {label.Input, "label.in", "currency", TypeCurrency},
		{label.Input, "label.in", "label_format", TypeString}, {label.Input, "label.in", "label_size", TypeString},
		{label.Input, "label.in", "cod_amount", TypeDecimal}, {label.Input, "label.in", "idempotency_key", TypeString},
		{label.Output, "label.out", "label_status", TypeString}, {label.Output, "label.out", "carrier", TypeString},
		{label.Output, "label.out", "tracking_number", TypeString}, {label.Output, "label.out", "label_url", TypeString},
		{label.Output, "label.out", "shipment_id", TypeString}, {label.Output, "label.out", "label_format", TypeString},
		{label.Output, "label.out", "tracking_url", TypeString}, {label.Output, "label.out", "cost", TypeDecimal},
		{label.Output, "label.out", "currency", TypeCurrency}, {label.Output, "label.out", "status", TypeString},
	}
	for _, f := range frozen {
		got, ok := f.s.Field(f.field)
		if !ok || got.Type != f.typ {
			t.Errorf("%s.%s: present=%v type=%q, want %q", f.name, f.field, ok, got.Type, f.typ)
		}
	}
	if d, _ := ratesOutF.Of.Field("delivery_type"); strings.Join(d.Enum, ",") != "door,branch,same_day,on_demand,pickup" {
		t.Errorf("delivery_type values changed: %v", d.Enum)
	}
	if strings.Join(DeliveryTypes, ",") != "door,branch,same_day,on_demand,pickup" {
		t.Errorf("DeliveryTypes = %v", DeliveryTypes)
	}
	if l, _ := label.Output.Field("label_status"); strings.Join(l.Enum, ",") != "created,label_pending" {
		t.Errorf("label_status values changed: %v", l.Enum)
	}

	// New fields: all optional, exact list (the one the ADR publishes).
	newRatesIn := "market,subtotal_basis,origins,delivery_types,origin_configured"
	newRates := "list_cost,free_reason,rule_ref,delivery_window,pickup_point,insurance,fallback,quote_ref,quoted_at,quote_hash,origin_location_id,billable_weight_kg,parcels"
	newLabelIn := "purpose,shipment_ref,insurance_value,quote_ref,quote_hash,origin_location_id"
	newLabelOut := "cost_tax,estimated_delivery,insurance_cost,billable_weight_kg,parcels"
	for _, tc := range []struct {
		s     *Schema
		names string
	}{{rates.Input, newRatesIn}, {ratesOutF.Of, newRates}, {label.Input, newLabelIn}, {label.Output, newLabelOut}} {
		for _, n := range strings.Split(tc.names, ",") {
			f, ok := tc.s.Field(n)
			if !ok || f.Required {
				t.Errorf("new field %s: present=%v required=%v", n, ok, f.Required)
			}
		}
	}
	if d, _ := ratesOutF.Of.Field("free_reason"); strings.Join(d.Enum, ",") != "none,rate_free,rate_free_over,staff_override,pickup_free" {
		t.Errorf("provider free reasons = %v", d.Enum)
	}
	// Append-only enumerations (a published prefix never changes).
	prefix := func(name string, got, want []string) {
		if len(got) < len(want) || strings.Join(got[:len(want)], ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want prefix %v", name, got, want)
		}
	}
	prefix("ShippingFreeReasons", ShippingFreeReasons, strings.Split("none,rate_free,rate_free_over,market_policy,shop_threshold,coupon,staff_override,pickup_free", ","))
	prefix("ShippingPurposes", ShippingPurposes, []string{"outbound", "return"})
	for _, r := range ShippingRateFreeReasons {
		if !contains(ShippingFreeReasons, r) {
			t.Errorf("provider free reason %q missing from the order-level set", r)
		}
	}
	for _, r := range []string{"coupon", "market_policy", "shop_threshold"} {
		if contains(ShippingRateFreeReasons, r) {
			t.Errorf("%s is decided by the cart, not by a provider", r)
		}
	}
}

func TestShippingContractFlagsUnchanged(t *testing.T) {
	rates := mustContract(t, CommerceShippingRates)
	if !rates.Fanout() || rates.Timeout != ShippingRatesTimeout || IdempotentContracts[CommerceShippingRates] || PinnedContracts[CommerceShippingRates] {
		t.Errorf("rates: fanout=%v timeout=%v idempotent=%v pinned=%v", rates.Fanout(), rates.Timeout, IdempotentContracts[CommerceShippingRates], PinnedContracts[CommerceShippingRates])
	}
	label := mustContract(t, CommerceShippingLabel)
	if label.Fanout() || !IdempotentContracts[CommerceShippingLabel] || PinnedContracts[CommerceShippingLabel] {
		t.Errorf("label: fanout=%v idempotent=%v pinned=%v", label.Fanout(), IdempotentContracts[CommerceShippingLabel], PinnedContracts[CommerceShippingLabel])
	}
	for _, k := range []string{CommerceShippingRates, CommerceShippingLabel} {
		if !ValidKey(k) || strings.Contains(k, "@") {
			t.Errorf("key %q", k)
		}
		c := mustContract(t, k)
		checkNoFloatMoney(t, k, c.Input)
		checkNoFloatMoney(t, k, c.Output)
	}
	if ValidKey("commerce.shipping.rates@1") {
		t.Error("a versioned key must stay invalid")
	}
	// Both events are untouched.
	for name, want := range map[string]string{CommerceShippingTrackingUpdated: "commerce.shipping", CommerceFulfillmentUpdated: "commerce.fulfillment"} {
		if e, ok := LookupEvent(name); !ok || e.EmittedBy != want {
			t.Errorf("event %s: %+v", name, e)
		}
	}
}

func TestShippingRatesInputTable(t *testing.T) {
	c := mustContract(t, CommerceShippingRates)
	with := func(k string, v any) map[string]any {
		in := ratesInput()
		in[k] = v
		return in
	}
	origin := func(extra map[string]any) map[string]any {
		o := map[string]any{"location_id": "loc-1", "postal_code": "44100", "country": "MX"}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	cases := []struct {
		name string
		in   map[string]any
		ok   bool
	}{
		{"market + basis + flags", func() map[string]any {
			in := with("market", "mx")
			in["subtotal_basis"] = "945.00"
			in["origin_configured"] = true
			in["delivery_types"] = []any{"door", "pickup"}
			return in
		}(), true},
		{"multi-origin", with("origins", []any{origin(nil), origin(map[string]any{"location_id": "loc-2", "postal_code": "64000"})}), true},
		{"items with weight and size", with("items", []any{map[string]any{"sku": "A", "quantity": 2, "weight_kg": "0.907", "length_cm": "12", "width_cm": "12", "height_cm": "20", "requires_shipping": true}}), true},
		{"service item without shipping", with("items", []any{map[string]any{"sku": "GC", "quantity": 1, "requires_shipping": false}}), true},
		{"float subtotal_basis", with("subtotal_basis", 945.0), false},
		{"non-decimal subtotal_basis", with("subtotal_basis", "945,00"), false},
		{"unknown delivery type", with("delivery_types", []any{"home"}), false},
		{"pickup_point is not a kernel delivery type", with("delivery_types", []any{"pickup_point"}), false},
		{"origin with a wrong typed field", with("origins", []any{origin(map[string]any{"country": "mexico"})}), false},
		{"float item weight", with("items", []any{map[string]any{"sku": "A", "quantity": 1, "weight_kg": 0.9}}), false},
		{"requires_shipping as string", with("items", []any{map[string]any{"sku": "A", "quantity": 1, "requires_shipping": "no"}}), false},
		{"origin_configured as string", with("origin_configured", "yes"), false},
		{"undeclared field passes (inputs are open)", with("insured", true), true},
		{"x_ extension", with("x_carrier_hint", "fedex"), true},
	}
	for _, tc := range cases {
		if errs := c.CheckInput(tc.in); (len(errs) == 0) != tc.ok {
			t.Errorf("%s: ok=%v errs=%v", tc.name, tc.ok, errs)
		}
	}
}

// Inputs are open (nested required keys are the provider's to enforce), so the
// shape of an origin is checked against its own schema.
func TestShippingOriginSchema(t *testing.T) {
	for name, tc := range map[string]struct {
		o  map[string]any
		ok bool
	}{
		"address + location":   {map[string]any{"location_id": "l1", "postal_code": "44100", "country": "MX", "city": "Guadalajara"}, true},
		"no location_id":       {map[string]any{"postal_code": "44100", "country": "MX"}, false},
		"no country":           {map[string]any{"location_id": "l1", "postal_code": "44100"}, false},
		"lower-case country":   {map[string]any{"location_id": "l1", "postal_code": "44100", "country": "mx"}, false},
		"undeclared in strict": {map[string]any{"location_id": "l1", "postal_code": "1", "country": "MX", "lat": 1}, false},
	} {
		if errs := ShippingOriginSchema.Validate(tc.o); (len(errs) == 0) != tc.ok {
			t.Errorf("%s: ok=%v errs=%v", name, tc.ok, errs)
		}
	}
}

func TestShippingRatesOutputTable(t *testing.T) {
	c := mustContract(t, CommerceShippingRates)
	full := `"list_cost":"159.00","free_reason":"rate_free_over","rule_ref":{"kind":"shipping_flat_rate","id":"5f1c","version":"3"},
	  "delivery_window":{"from":"2026-10-23","to":"2026-10-28"},"eta_days_min":3,"eta_days_max":6,"delivery_type":"branch","pickup":true,
	  "pickup_point":{"id":"p1","name":"Sucursal Centro","address":{"line1":"Av. Vallarta 123","postal_code":"44100","country":"MX"},"hours":"L-V 9-18","lat":20.67,"lng":-103.35},
	  "insurance":{"available":true,"included":false,"cost":"12.50"},"fallback":false,"quote_ref":"q-77","quoted_at":"2026-10-20T21:40:11Z",
	  "quote_hash":"` + hash64 + `","origin_location_id":"loc-1","billable_weight_kg":"2.721",
	  "parcels":[{"weight_kg":"2.721","length_cm":"12","width_cm":"12","height_cm":"20","quantity":3}],"expires_at":"2026-10-20T22:10:00Z","x_raw":"1"`
	cases := []struct {
		name, out string
		ok        bool
	}{
		{"legacy minimal", rateOut(""), true},
		{"every new field", rateOut(full), true},
		{"free at list price zero", rateOut(`"list_cost":"0.00","free_reason":"rate_free"`), true},
		{"not free", rateOut(`"list_cost":"159.00","free_reason":"none"`), true},
		{"pickup_free", rateOut(`"free_reason":"pickup_free","delivery_type":"pickup"`), true},
		{"fallback rate", rateOut(`"fallback":true`), true},
		{"float list_cost", rateOut(`"list_cost":159.0`), false},
		{"negative-looking text", rateOut(`"list_cost":"free"`), false},
		{"coupon is the cart's", rateOut(`"free_reason":"coupon"`), false},
		{"market_policy is the cart's", rateOut(`"free_reason":"market_policy"`), false},
		{"shop_threshold is the cart's", rateOut(`"free_reason":"shop_threshold"`), false},
		{"unknown free_reason", rateOut(`"free_reason":"promo"`), false},
		{"rule_ref without id", rateOut(`"rule_ref":{"kind":"shipping_flat_rate"}`), false},
		{"window without to", rateOut(`"delivery_window":{"from":"2026-10-23"}`), false},
		{"window not a date", rateOut(`"delivery_window":{"from":"23/10/2026","to":"2026-10-28"}`), false},
		{"window with time", rateOut(`"delivery_window":{"from":"2026-10-23T00:00:00Z","to":"2026-10-28"}`), false},
		{"pickup_point without name", rateOut(`"pickup_point":{"id":"p1"}`), false},
		{"pickup_point lat out of range", rateOut(`"pickup_point":{"id":"p1","name":"S","lat":91}`), false},
		{"insurance float cost", rateOut(`"insurance":{"cost":12.5}`), false},
		{"insurance unknown field", rateOut(`"insurance":{"premium":"1"}`), false},
		{"hash without prefix", rateOut(`"quote_hash":"0123"`), false},
		{"hash upper case", rateOut(`"quote_hash":"sha256:` + strings.Repeat("A", 64) + `"`), false},
		{"hash wrong length", rateOut(`"quote_hash":"sha256:abcd"`), false},
		{"unknown delivery_type", rateOut(`"delivery_type":"home"`), false},
		{"parcel without weight", rateOut(`"parcels":[{"length_cm":"10"}]`), false},
		{"float billable weight", rateOut(`"billable_weight_kg":2.7`), false},
		{"fallback as string", rateOut(`"fallback":"yes"`), false},
		{"undeclared field", rateOut(`"discount":"1"`), false},
	}
	for _, tc := range cases {
		if errs := c.ValidateOutput([]byte(tc.out)); (len(errs) == 0) != tc.ok {
			t.Errorf("%s: ok=%v errs=%v", tc.name, tc.ok, errs)
		}
	}
	for _, r := range ShippingRateFreeReasons {
		if errs := c.ValidateOutput([]byte(rateOut(`"free_reason":"` + r + `"`))); len(errs) != 0 {
			t.Errorf("free_reason %s rejected: %v", r, errs)
		}
	}
	for _, d := range DeliveryTypes {
		if errs := c.ValidateOutput([]byte(rateOut(`"delivery_type":"` + d + `"`))); len(errs) != 0 {
			t.Errorf("delivery_type %s rejected: %v", d, errs)
		}
	}
}

func TestShippingLabelTable(t *testing.T) {
	c := mustContract(t, CommerceShippingLabel)
	in := func(extra map[string]any) map[string]any {
		m := map[string]any{"rate_id": "r1", "order_ref": "SO-1", "idempotency_key": "shipment:1:label:1"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	inputs := []struct {
		name string
		in   map[string]any
		ok   bool
	}{
		{"legacy", in(nil), true},
		{"return label", in(map[string]any{"purpose": "return", "shipment_ref": "shp-9"}), true},
		{"insured, with the quote it came from", in(map[string]any{"insurance_value": "749.00", "quote_ref": "q-77", "quote_hash": hash64, "origin_location_id": "loc-1"}), true},
		{"unknown purpose", in(map[string]any{"purpose": "exchange"}), false},
		{"float insurance", in(map[string]any{"insurance_value": 749.0}), false},
		{"bad quote hash", in(map[string]any{"quote_hash": "md5:abc"}), false},
		{"no idempotency key", func() map[string]any { m := in(nil); delete(m, "idempotency_key"); return m }(), false},
		{"undeclared field passes (inputs are open)", in(map[string]any{"insured": true}), true},
	}
	for _, tc := range inputs {
		if errs := c.CheckInput(tc.in); (len(errs) == 0) != tc.ok {
			t.Errorf("input %s: ok=%v errs=%v", tc.name, tc.ok, errs)
		}
	}
	base := `"label_status":"created","carrier":"fedex"`
	outputs := []struct {
		name, out string
		ok        bool
	}{
		{"legacy", `{` + base + `}`, true},
		{"every new field", `{` + base + `,"cost":"120.00","currency":"MXN","cost_tax":"16.55","estimated_delivery":"2026-10-27","insurance_cost":"12.50","billable_weight_kg":"2.721","parcels":[{"weight_kg":"2.721","length_cm":"30","width_cm":"20","height_cm":"15"}]}`, true},
		{"pending label keeps working", `{"label_status":"label_pending","carrier":"envia"}`, true},
		{"float cost_tax", `{` + base + `,"cost_tax":16.5}`, false},
		{"datetime as delivery date", `{` + base + `,"estimated_delivery":"2026-10-27T10:00:00Z"}`, false},
		{"parcel without weight", `{` + base + `,"parcels":[{"length_cm":"3"}]}`, false},
		{"undeclared field", `{` + base + `,"surcharge":"1"}`, false},
	}
	for _, tc := range outputs {
		if errs := c.ValidateOutput([]byte(tc.out)); (len(errs) == 0) != tc.ok {
			t.Errorf("output %s: ok=%v errs=%v", tc.name, tc.ok, errs)
		}
	}
}

// Rates fan out to every provider of the market (or the subset the org chose);
// label goes to ONE provider by the normal selection rules.
func TestShippingSelectionByOrgAndMarket(t *testing.T) {
	rates := mustContract(t, CommerceShippingRates)
	label := mustContract(t, CommerceShippingLabel)
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	us := Market{Code: "us", Countries: []string{"US"}}
	flat := Provider{Addon: "shipping_flat", Default: true}
	envia := Provider{Addon: "envia", Countries: []string{"MX"}}
	skydropx := Provider{Addon: "skydropx", Countries: []string{"MX", "US"}}
	ups := Provider{Addon: "ups", Countries: []string{"US"}}
	all := []Provider{ups, skydropx, flat, envia}
	addons := func(ps []Provider) string {
		var s []string
		for _, p := range ps {
			s = append(s, p.Addon)
		}
		return strings.Join(s, ",")
	}

	// Fan-out: everyone serving the market, in stable (addon) order.
	for _, tc := range []struct {
		m    Market
		want string
	}{{mx, "envia,shipping_flat,skydropx"}, {us, "shipping_flat,skydropx,ups"}} {
		got, err := Select(rates, all, tc.m)
		if err != nil || addons(got) != tc.want {
			t.Errorf("rates %s: %s %v, want %s", tc.m.Code, addons(got), err, tc.want)
		}
	}
	// The org narrows the market to the carriers it picked.
	picked := []Provider{{Addon: "envia", Markets: []string{"mx"}}, flat, skydropx}
	if got, err := Select(rates, picked, mx); err != nil || addons(got) != "envia" {
		t.Errorf("rates with a market selection: %s %v", addons(got), err)
	}
	if _, err := Select(rates, []Provider{ups}, mx); err == nil {
		t.Error("no provider serves mx: want an error")
	}
	// Label: ONE provider. Default wins; a market selection wins over the default.
	if got, err := Select(label, all, mx); err != nil || addons(got) != "shipping_flat" {
		t.Errorf("label default: %s %v", addons(got), err)
	}
	if got, err := Select(label, []Provider{flat, {Addon: "envia", Markets: []string{"mx"}}}, mx); err != nil || addons(got) != "envia" {
		t.Errorf("label market selection: %s %v", addons(got), err)
	}
	if _, err := Select(label, []Provider{envia, skydropx}, mx); err == nil {
		t.Error("label with two eligible carriers and no default must be ambiguous, not a guess")
	}
}

// Old and new providers answer the same fan-out; the invalid one is isolated.
func TestShippingFanoutMixesOldAndNewProviders(t *testing.T) {
	g := &Engine{
		Source: pinSource(
			Provider{Addon: "envia", Countries: []string{"MX"}},
			Provider{Addon: "shipping_flat", Default: true},
			Provider{Addon: "skydropx", Countries: []string{"MX"}},
			Provider{Addon: "ups", Countries: []string{"US"}},
		),
		Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
			switch call.Provider.Addon {
			case "envia": // built before the evolution
				return json.RawMessage(`{"rates":[` + rate("e1", "estafeta", "120.00") + `]}`), nil
			case "shipping_flat": // adopted the new fields
				return json.RawMessage(rateOut(`"list_cost":"0.00","free_reason":"rate_free","delivery_window":{"from":"2026-10-23","to":"2026-10-28"},"eta_days_min":3,"eta_days_max":6,"delivery_type":"door"`)), nil
			case "skydropx": // new field with the wrong type
				return json.RawMessage(rateOut(`"list_cost":159.0`)), nil
			}
			t.Errorf("%s must not be called for market mx", call.Provider.Addon)
			return nil, nil
		},
	}
	res, err := g.Query(context.Background(), Query{Org: org, Market: Market{Code: "mx", Countries: []string{"MX"}}, Capability: CommerceShippingRates, Input: ratesInput()})
	if err != nil {
		t.Fatal(err)
	}
	ok := map[string]bool{}
	for _, r := range res.Outputs() {
		ok[r.Provider] = true
	}
	if !ok["envia"] || !ok["shipping_flat"] || len(ok) != 2 {
		t.Fatalf("answers = %v", ok)
	}
	failed := res.Failed()
	if len(failed) != 1 || failed[0].Provider != "skydropx" || failed[0].ErrorCode() != "invalid_output" {
		t.Fatalf("failed = %+v", failed)
	}
	if !strings.Contains(fmt.Sprint(failed[0].Err), "list_cost") {
		t.Fatalf("invalid output not explained: %v", failed[0].Err)
	}
}

// A single provider (the usual shop: only shipping_flat) answers the fan-out
// with one result and no ambiguity.
func TestShippingSingleProvider(t *testing.T) {
	g := &Engine{
		Source: pinSource(Provider{Addon: "shipping_flat"}),
		Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
			return json.RawMessage(rateOut(`"list_cost":"159.00","free_reason":"rate_free_over","rule_ref":{"kind":"shipping_flat_rate","id":"5f1c"}`)), nil
		},
	}
	res, err := g.Query(context.Background(), Query{Org: org, Market: Market{Code: "mx", Countries: []string{"MX"}}, Capability: CommerceShippingRates, Input: ratesInput()})
	if err != nil || len(res.Outputs()) != 1 || len(res.Failed()) != 0 {
		t.Fatalf("single provider: %v %+v", err, res)
	}
}
