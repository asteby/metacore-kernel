package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

var org = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func ratesInput() map[string]any {
	addr := func(cp string) map[string]any { return map[string]any{"postal_code": cp, "country": "MX"} }
	return map[string]any{
		"origin":      addr("64000"),
		"destination": addr("06600"),
		"parcels":     []any{map[string]any{"weight_kg": "1.5", "length_cm": "30"}},
		"currency":    "MXN",
	}
}

func rate(id, carrier, cost string) string {
	return fmt.Sprintf(`{"id":%q,"carrier":%q,"service":"Express","cost":%q,"currency":"MXN","eta_days_min":1,"eta_days_max":2}`, id, carrier, cost)
}

func ratesOutput(rates ...string) json.RawMessage {
	return json.RawMessage(`{"rates":[` + strings.Join(rates, ",") + `]}`)
}

func mustContract(t *testing.T, key string) Contract {
	t.Helper()
	c, ok := Lookup(key)
	if !ok {
		t.Fatalf("contract %s not registered", key)
	}
	return c
}

func TestExecutorFanoutPartialResultsTimeoutAndErrorIsolation(t *testing.T) {
	c := mustContract(t, CommerceShippingRates)
	providers := []Provider{
		{Addon: "envia"},
		{Addon: "shipping_flat"},
		{Addon: "skydropx", Input: map[string]string{"zip_to": "payload.destination"}},
		{Addon: "slowpost", Timeout: 30 * time.Millisecond},
		{Addon: "broken"},
		{Addon: "liar"},
		{Addon: "panicky"},
	}
	var seenSkydropx atomic.Value
	invoke := func(ctx context.Context, call Call) (json.RawMessage, error) {
		if call.Org != org || call.Market != "mx" || call.Capability != CommerceShippingRates {
			t.Errorf("call scope lost: %+v", call)
		}
		switch call.Provider.Addon {
		case "envia":
			return ratesOutput(rate("e1", "estafeta", "120.00")), nil
		case "shipping_flat":
			return ratesOutput(rate("flat", "local", "99")), nil
		case "skydropx":
			seenSkydropx.Store(string(call.Payload))
			return ratesOutput(rate("s1", "fedex", "150.50"), rate("s2", "dhl", "180.00")), nil
		case "slowpost":
			<-ctx.Done() // honours the deadline
			time.Sleep(50 * time.Millisecond)
			return ratesOutput(rate("late", "x", "1")), nil
		case "broken":
			return nil, errors.New("401 bad credentials")
		case "liar":
			return json.RawMessage(`{"rates":[{"id":"x","carrier":"c","service":"s","cost":12.5,"currency":"mxn"}]}`), nil
		case "panicky":
			panic("nil map")
		}
		return nil, nil
	}
	start := time.Now()
	res, err := Executor{Timeout: time.Second}.Run(context.Background(), c, org, "mx", providers, ratesInput(), invoke)
	if err != nil {
		t.Fatalf("fan-out must not fail as a whole: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("fan-out waited for the slow provider: %s", time.Since(start))
	}
	if len(res.Results) != len(providers) {
		t.Fatalf("want one result per provider, got %d", len(res.Results))
	}
	ok := map[string]bool{}
	for _, r := range res.Outputs() {
		ok[r.Provider] = true
	}
	if !ok["envia"] || !ok["shipping_flat"] || !ok["skydropx"] || len(ok) != 3 {
		t.Fatalf("successful providers = %v", ok)
	}
	codes := map[string]string{}
	for _, r := range res.Failed() {
		codes[r.Provider] = r.ErrorCode()
	}
	want := map[string]string{"slowpost": "timeout", "broken": "provider_error", "liar": "invalid_output", "panicky": "panic"}
	for p, code := range want {
		if codes[p] != code {
			t.Fatalf("%s: code %q, want %q (all: %v)", p, codes[p], code, codes)
		}
	}
	// Results keep provider order.
	for i, p := range providers {
		if res.Results[i].Provider != p.Addon {
			t.Fatalf("result %d is %s, want %s", i, res.Results[i].Provider, p.Addon)
		}
	}
	// The provider's own input mapping was applied.
	if s, _ := seenSkydropx.Load().(string); !strings.Contains(s, `"zip_to":{"country":"MX","postal_code":"06600"}`) {
		t.Fatalf("provider mapping not applied: %s", s)
	}
	// Wire envelope.
	raw, _ := json.Marshal(res)
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Results) != 3 || len(env.Errors) != 4 || env.Market != "mx" {
		t.Fatalf("envelope = %s (%v)", raw, err)
	}
	if !strings.Contains(string(raw), `"code":"invalid_output"`) || !strings.Contains(string(raw), "rates[0].cost") {
		t.Fatalf("invalid output not explained: %s", raw)
	}
}

func TestExecutorOneInvokesOnlyTheSelectedProvider(t *testing.T) {
	c := mustContract(t, CommercePaymentsSession)
	var calls int32
	invoke := func(_ context.Context, call Call) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		if call.Provider.Addon != "mercadopago" {
			t.Errorf("invoked %s", call.Provider.Addon)
		}
		var in map[string]any
		_ = json.Unmarshal(call.Payload, &in)
		if in["external_reference"] != "ORD-1" || in["amount"] != "1499.00" {
			t.Errorf("payload = %v", in)
		}
		return json.RawMessage(`{"session_id":"pref_1","status":"created","redirect_url":"https://mp.example/checkout","x_init_point":"…"}`), nil
	}
	input := map[string]any{"order_ref": "ORD-1", "amount": "1499.00", "currency": "MXN", "return_url": "https://shop.mx/thanks", "idempotency_key": "ord-1-pay"}
	res, err := Executor{}.Run(context.Background(), c, org, "", []Provider{
		{Addon: "mercadopago", Input: map[string]string{"external_reference": "payload.order_ref"}},
		{Addon: "stripe"},
	}, input, invoke)
	if err != nil {
		t.Fatal(err)
	}
	out, err := res.One()
	if err != nil || !strings.Contains(string(out), "pref_1") || calls != 1 {
		t.Fatalf("one: %s %v calls=%d", out, err, calls)
	}
}

func TestExecutorRejectsBeforeInvoking(t *testing.T) {
	never := func(context.Context, Call) (json.RawMessage, error) {
		t.Fatal("must not invoke")
		return nil, nil
	}
	e := Executor{}
	ctx := context.Background()
	if _, err := e.Run(ctx, mustContract(t, MessagingWhatsAppSend), org, "", []Provider{{Addon: "a"}}, map[string]any{"to": "1"}, never); !errors.Is(err, ErrNotQuery) {
		t.Fatalf("action contract: %v", err)
	}
	bad := ratesInput()
	delete(bad, "parcels")
	bad["currency"] = "pesos"
	_, err := e.Run(ctx, mustContract(t, CommerceShippingRates), org, "", []Provider{{Addon: "a"}}, bad, never)
	var ie *InputError
	if !errors.As(err, &ie) || !errors.Is(err, ErrInvalidInput) || len(ie.Problems) != 2 {
		t.Fatalf("input errors: %v", err)
	}
	if _, err := e.Run(ctx, mustContract(t, CommerceShippingRates), org, "", nil, ratesInput(), never); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("no providers: %v", err)
	}
}

func TestExecutorParentCancelAndMaxParallel(t *testing.T) {
	c := mustContract(t, CommerceShippingRates)
	var inflight, peak int32
	invoke := func(ctx context.Context, _ Call) (json.RawMessage, error) {
		n := atomic.AddInt32(&inflight, 1)
		defer atomic.AddInt32(&inflight, -1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		return ratesOutput(rate("r", "c", "1")), nil
	}
	ps := []Provider{{Addon: "a"}, {Addon: "b"}, {Addon: "c"}, {Addon: "d"}}
	res, err := Executor{MaxParallel: 2}.Run(context.Background(), c, org, "", ps, ratesInput(), invoke)
	if err != nil || len(res.Outputs()) != 4 || peak > 2 {
		t.Fatalf("max parallel: outputs=%d peak=%d err=%v", len(res.Outputs()), peak, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = Executor{}.Run(ctx, c, org, "", ps[:1], ratesInput(), func(ctx context.Context, _ Call) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil || res.Results[0].ErrorCode() != "canceled" {
		t.Fatalf("canceled parent: %v %+v", err, res.Results)
	}
}

func TestSelectByMarket(t *testing.T) {
	pay := mustContract(t, CommercePaymentsSession)
	mp := Provider{Addon: "mercadopago", Countries: []string{"MX", "AR"}, Markets: []string{"mx"}}
	stripe := Provider{Addon: "stripe", Default: true}
	manual := Provider{Addon: "manual"}
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	us := Market{Code: "us", Countries: []string{"US"}}

	pick := func(c Contract, ps []Provider, m Market) string {
		t.Helper()
		got, err := Select(c, ps, m)
		if err != nil {
			return "err:" + err.Error()
		}
		var keys []string
		for _, p := range got {
			keys = append(keys, p.Addon)
		}
		return strings.Join(keys, ",")
	}
	all := []Provider{stripe, manual, mp}
	if got := pick(pay, all, mx); got != "mercadopago" {
		t.Fatalf("mx market selection: %s", got)
	}
	if got := pick(pay, all, us); got != "stripe" {
		t.Fatalf("us falls back to org default: %s", got)
	}
	if got := pick(pay, all, Market{}); got != "stripe" {
		t.Fatalf("org level: %s", got)
	}
	// Countries filter: mercadopago does not serve US, so only manual is left
	// once stripe is gone.
	if got := pick(pay, []Provider{mp, manual}, us); got != "manual" {
		t.Fatalf("country filter: %s", got)
	}
	_, err := Select(pay, []Provider{{Addon: "a"}, {Addon: "b"}}, Market{})
	var se *SelectionError
	if !errors.Is(err, ErrAmbiguousProvider) || !errors.As(err, &se) || strings.Join(se.Candidates, ",") != "a,b" {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := Select(pay, []Provider{mp}, us); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("no eligible provider: %v", err)
	}

	rates := mustContract(t, CommerceShippingRates)
	sky := Provider{Addon: "skydropx", Countries: []string{"MX"}}
	flat := Provider{Addon: "shipping_flat"}
	emissary := Provider{Addon: "emissary", Countries: []string{"MX"}}
	if got := pick(rates, []Provider{sky, flat, emissary}, us); got != "shipping_flat" {
		t.Fatalf("fan-out country filter: %s", got)
	}
	if got := pick(rates, []Provider{sky, flat, emissary}, mx); got != "emissary,shipping_flat,skydropx" {
		t.Fatalf("fan-out all eligible: %s", got)
	}
	emissary.Markets = []string{"mx"}
	if got := pick(rates, []Provider{sky, flat, emissary}, mx); got != "emissary" {
		t.Fatalf("fan-out market subset: %s", got)
	}
}

func TestEngineQuery(t *testing.T) {
	src := ProviderSourceFunc(func(_ context.Context, o uuid.UUID, key string) ([]Provider, error) {
		if o != org || key != FiscalInvoiceCreate {
			return nil, fmt.Errorf("unexpected %s %s", o, key)
		}
		return []Provider{{Addon: "factura_com", Countries: []string{"MX"}}}, nil
	})
	g := &Engine{Source: src, Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
		return json.RawMessage(`{"invoice_id":"inv_1","status":"issued","fiscal_uuid":"ABC","total":"116.00","currency":"MXN"}`), nil
	}}
	var q Querier = g
	in := map[string]any{
		"order_ref": "ORD-9", "currency": "MXN", "idempotency_key": "ord-9-cfdi",
		"customer": map[string]any{"tax_id": "XAXX010101000", "name": "PUBLICO EN GENERAL"},
		"lines":    []any{map[string]any{"id": "1", "description": "Balata", "quantity": "1", "unit_price": "100.00"}},
	}
	res, err := q.Query(context.Background(), Query{Org: org, Market: Market{Code: "mx", Countries: []string{"MX"}}, Capability: FiscalInvoiceCreate, Input: in})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := res.One(); err != nil || !strings.Contains(string(out), "inv_1") {
		t.Fatalf("one = %s %v", out, err)
	}
	if _, err := q.Query(context.Background(), Query{Org: org, Market: Market{Code: "us", Countries: []string{"US"}}, Capability: FiscalInvoiceCreate, Input: in}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("CFDI provider must not serve US: %v", err)
	}
	if _, err := q.Query(context.Background(), Query{Org: org, Capability: "acme.unknown.thing"}); !errors.Is(err, ErrNotQuery) {
		t.Fatalf("unknown contract: %v", err)
	}
}

func TestSelectionFromSettings(t *testing.T) {
	var settings map[string]any
	_ = json.Unmarshal([]byte(`{"_capability_defaults":["commerce.payments.session"],"_capability_markets":{"commerce.payments.session":["mx"],"commerce.tax.calculate":["us"]}}`), &settings)
	def, markets := SelectionFromSettings(settings, CommercePaymentsSession)
	if !def || strings.Join(markets, ",") != "mx" {
		t.Fatalf("payments: %v %v", def, markets)
	}
	def, markets = SelectionFromSettings(settings, CommerceShippingRates)
	if def || markets != nil {
		t.Fatalf("rates: %v %v", def, markets)
	}
}

func pinSource(providers ...Provider) ProviderSource {
	return ProviderSourceFunc(func(context.Context, uuid.UUID, string) ([]Provider, error) { return providers, nil })
}

func TestPinnedConfirmReachesOnlyThatProvider(t *testing.T) {
	var hit []string
	g := &Engine{
		// MP is the org default; Stripe is selected for "us" and ambiguity
		// would otherwise route a Stripe webhook to the default.
		Source: pinSource(
			Provider{Addon: "mercadopago", Default: true},
			Provider{Addon: "stripe", Markets: []string{"us"}},
			Provider{Addon: "openpay"},
		),
		Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
			hit = append(hit, call.Provider.Addon)
			return json.RawMessage(`{"order_ref":"O1","status":"paid","amount":"10.00","currency":"USD","provider_ref":"pi_1"}`), nil
		},
	}
	in := map[string]any{"payload": map[string]any{"id": "evt_1"}}
	if _, err := g.Query(context.Background(), Query{Org: org, Capability: CommercePaymentsConfirm, Input: in, Provider: "stripe"}); err != nil {
		t.Fatal(err)
	}
	if len(hit) != 1 || hit[0] != "stripe" {
		t.Fatalf("a Stripe event must reach only Stripe: %v", hit)
	}
	// Unknown / not installed addon.
	if _, err := g.Query(context.Background(), Query{Org: org, Capability: CommercePaymentsConfirm, Input: in, Provider: "paypal"}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("unknown addon: %v", err)
	}
	// Disabled addon = absent from the source.
	g.Source = pinSource(Provider{Addon: "mercadopago", Default: true})
	if _, err := g.Query(context.Background(), Query{Org: org, Capability: CommercePaymentsConfirm, Input: in, Provider: "stripe"}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("disabled addon: %v", err)
	}
	if len(hit) != 1 {
		t.Fatalf("rejected pins must not invoke: %v", hit)
	}
	// Country filter still applies.
	g.Source = pinSource(Provider{Addon: "stripe", Countries: []string{"US"}})
	_, err := g.Query(context.Background(), Query{Org: org, Market: Market{Code: "mx", Countries: []string{"MX"}}, Capability: CommercePaymentsConfirm, Input: in, Provider: "stripe"})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("country filter: %v", err)
	}
}

func TestPinRequiredContractsRefuseWithoutProvider(t *testing.T) {
	g := &Engine{
		Source: pinSource(Provider{Addon: "stripe", Default: true}),
		Invoke: func(context.Context, Call) (json.RawMessage, error) { t.Error("must not invoke"); return nil, nil },
	}
	for _, key := range []string{
		CommercePaymentsConfirm, CommercePaymentsStatus, CommercePaymentsRefund,
		CommerceShippingCancel, CommerceShippingTrack, FiscalInvoiceCancel,
		CommerceTaxCommit, CommerceTaxReverse,
	} {
		c := mustContract(t, key)
		if !c.PinRequired {
			t.Errorf("%s must require a pinned provider", key)
		}
		if _, err := g.Query(context.Background(), Query{Org: org, Capability: key}); !errors.Is(err, ErrProviderRequired) {
			t.Errorf("%s without provider: %v", key, err)
		}
	}
	// Creation contracts keep the normal selection.
	if mustContract(t, CommercePaymentsSession).PinRequired {
		t.Error("payments.session selects, it is not pinned")
	}
}

func TestMutatingContractsRequireIdempotencyKey(t *testing.T) {
	for key := range IdempotentContracts {
		c := mustContract(t, key)
		if !c.Idempotent || c.Timeout < MutatingTimeout {
			t.Errorf("%s: idempotent=%v timeout=%s", key, c.Idempotent, c.Timeout)
		}
		f, ok := c.Input.Field("idempotency_key")
		if !ok || !f.Required {
			t.Errorf("%s: idempotency_key must be required", key)
		}
	}
	c := mustContract(t, CommerceShippingLabel)
	// Even with a contract whose schema forgot the field, the Executor refuses.
	c.Input = nil
	c.Required = []string{"rate_id", "order_ref"}
	_, err := Executor{}.Run(context.Background(), c, org, "", []Provider{{Addon: "x"}},
		map[string]any{"rate_id": "r", "order_ref": "O", "idempotency_key": "  "},
		func(context.Context, Call) (json.RawMessage, error) { t.Error("must not invoke"); return nil, nil })
	if !errors.Is(err, ErrIdempotencyRequired) {
		t.Fatalf("blank key: %v", err)
	}
}

func TestTimedOutLabelRetriedWithSameKeyDoesNotDuplicate(t *testing.T) {
	c := mustContract(t, CommerceShippingLabel)
	c.Timeout = 20 * time.Millisecond
	var mu sync.Mutex
	labels := map[string]string{} // idempotency key -> tracking number
	var first int32
	invoke := func(ctx context.Context, call Call) (json.RawMessage, error) {
		if call.IdempotencyKey == "" {
			t.Error("key not handed to the provider")
		}
		if atomic.AddInt32(&first, 1) == 1 {
			time.Sleep(80 * time.Millisecond) // slower than the deadline, still completes
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := labels[call.IdempotencyKey]; !ok {
			labels[call.IdempotencyKey] = fmt.Sprintf("TRK%d", len(labels)+1)
		}
		return json.RawMessage(fmt.Sprintf(`{"label_status":"created","carrier":"fedex","tracking_number":%q}`, labels[call.IdempotencyKey])), nil
	}
	in := map[string]any{"rate_id": "r1", "order_ref": "O1", "idempotency_key": "ship-O1"}
	res, err := Executor{}.Run(context.Background(), c, org, "", []Provider{{Addon: "skydropx"}}, in, invoke)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Results[0]
	if !errors.Is(r.Err, ErrTimeout) || !r.Indeterminate() {
		t.Fatalf("timeout must be indeterminate: %v", r.Err)
	}
	if env := res.Envelope(); len(env.Errors) != 1 || !env.Errors[0].Indeterminate {
		t.Fatalf("envelope must flag it: %+v", env.Errors)
	}
	time.Sleep(100 * time.Millisecond) // the abandoned call completes
	c.Timeout = time.Second
	res, err = Executor{}.Run(context.Background(), c, org, "", []Provider{{Addon: "skydropx"}}, in, invoke)
	if err != nil || !res.Results[0].OK() {
		t.Fatalf("retry: %v %v", err, res.Results[0].Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(labels) != 1 {
		t.Fatalf("a retry with the same key created %d labels", len(labels))
	}
	// A non-mutating timeout is a plain timeout.
	rc := mustContract(t, CommerceShippingRates)
	rc.Timeout = 10 * time.Millisecond
	res, _ = Executor{}.Run(context.Background(), rc, org, "", []Provider{{Addon: "slow"}}, ratesInput(),
		func(ctx context.Context, _ Call) (json.RawMessage, error) { <-ctx.Done(); return nil, ctx.Err() })
	if res.Results[0].Indeterminate() {
		t.Fatal("rates timeout is not indeterminate")
	}
}
