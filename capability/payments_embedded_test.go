package capability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

var embeddedKeys = []string{
	CommercePaymentsSubmit, CommercePaymentsClient,
	CommercePaymentsNotificationInspect, CommercePaymentsNotificationResolve, CommercePaymentsHealth,
}

func TestEmbeddedPaymentKeys(t *testing.T) {
	want := map[string]string{
		CommercePaymentsSubmit:              "commerce.payments.submit",
		CommercePaymentsClient:              "commerce.payments.client",
		CommercePaymentsNotificationInspect: "commerce.payments.notification.inspect",
		CommercePaymentsNotificationResolve: "commerce.payments.notification.resolve",
		CommercePaymentsHealth:              "commerce.payments.health",
	}
	for got, w := range want {
		if got != w || !ValidKey(got) {
			t.Errorf("key %q (want %q) valid=%v", got, w, ValidKey(got))
		}
		c := mustContract(t, got)
		if !c.IsQuery() || c.Output == nil || c.Input == nil {
			t.Errorf("%s: query=%v input=%v output=%v", got, c.IsQuery(), c.Input != nil, c.Output != nil)
		}
		// Same domain as the other payment contracts: the money event policy
		// (providers of commerce.payments.* only) is unchanged.
		if !strings.HasPrefix(got, "commerce.payments.") || !ProvidesDomain([]string{got}, "commerce.payments") {
			t.Errorf("%s outside the payments domain", got)
		}
		checkNoFloatMoney(t, got, c.Input)
		checkNoFloatMoney(t, got, c.Output)
	}
	// Keys carry no version suffix: the package evolves by optional fields.
	for _, bad := range []string{"commerce.payments.submit@1", "commerce.payments..submit", "Commerce.payments.submit"} {
		if ValidKey(bad) {
			t.Errorf("%q must not be a valid key", bad)
		}
	}
}

func TestEmbeddedPaymentContractFlags(t *testing.T) {
	cases := []struct {
		key                string
		pinned, idempotent bool
	}{
		{CommercePaymentsSubmit, true, true},
		{CommercePaymentsClient, true, false},
		{CommercePaymentsNotificationInspect, true, false},
		{CommercePaymentsNotificationResolve, true, false},
		{CommercePaymentsHealth, true, false},
	}
	for _, c := range cases {
		ct := mustContract(t, c.key)
		if ct.Fanout() || ct.PinRequired != c.pinned || ct.Idempotent != c.idempotent {
			t.Errorf("%s: fanout=%v pinned=%v idempotent=%v", c.key, ct.Fanout(), ct.PinRequired, ct.Idempotent)
		}
		if c.idempotent && ct.Timeout != MutatingTimeout {
			t.Errorf("%s timeout %s, want MutatingTimeout", c.key, ct.Timeout)
		}
		if !c.idempotent && ct.Timeout != 0 {
			t.Errorf("%s read-only contract has a custom timeout %s", c.key, ct.Timeout)
		}
	}
	if !IdempotentContracts[CommercePaymentsSubmit] || !PinnedContracts[CommercePaymentsSubmit] {
		t.Fatal("submit must be in IdempotentContracts and PinnedContracts")
	}
}

func submitInput() map[string]any {
	return map[string]any{
		"session_id": "pref_1", "order_ref": "SO-1", "amount": "1499.00", "currency": "MXN",
		"method": "card", "idempotency_key": "pay-chk1-a1",
		"provider_data": map[string]any{"token": "tok_APRO", "payment_method_id": "master", "installments": 3},
	}
}

func TestSubmitInput(t *testing.T) {
	c := mustContract(t, CommercePaymentsSubmit)
	if got := strings.Join(c.Input.Required(), ","); got != "session_id,order_ref,amount,currency,method,provider_data,idempotency_key" {
		t.Fatalf("required input = %s", got)
	}
	full := submitInput()
	full["device_session_id"] = "dev-1"
	full["market"] = "mx"
	full["metadata"] = map[string]any{"checkout_id": "c1", "attempt": 2, "x_other": "ok"}
	full["expires_at"] = "2026-10-22T10:00:00Z"
	full["buyer"] = map[string]any{"email": "a@b.mx", "name": "Ana"}
	full["lines"] = []any{map[string]any{"id": "1", "quantity": 1, "unit_price": "1499.00"}}
	cash := submitInput()
	cash["method"] = "cash"
	cash["provider_data"] = map[string]any{"payment_method_id": "paycash", "payment_point": "7eleven"}

	cases := []struct {
		name string
		in   func() map[string]any
		ok   bool
	}{
		{"minimal card", submitInput, true},
		{"full", func() map[string]any { return full }, true},
		{"cash voucher", func() map[string]any { return cash }, true},
		{"bank transfer", func() map[string]any { m := submitInput(); m["method"] = "bank_transfer"; return m }, true},
		{"undeclared input passes", func() map[string]any { m := submitInput(); m["x_anything"] = 1; return m }, true},
		{"no idempotency key", func() map[string]any { m := submitInput(); delete(m, "idempotency_key"); return m }, false},
		{"blank idempotency key", func() map[string]any { m := submitInput(); m["idempotency_key"] = " "; return m }, false},
		{"no provider_data", func() map[string]any { m := submitInput(); delete(m, "provider_data"); return m }, false},
		{"no session", func() map[string]any { m := submitInput(); delete(m, "session_id"); return m }, false},
		{"no order_ref", func() map[string]any { m := submitInput(); delete(m, "order_ref"); return m }, false},
		{"float amount", func() map[string]any { m := submitInput(); m["amount"] = 1499.0; return m }, false},
		{"bad decimal", func() map[string]any { m := submitInput(); m["amount"] = "1,499.00"; return m }, false},
		{"lowercase currency", func() map[string]any { m := submitInput(); m["currency"] = "mxn"; return m }, false},
		{"unknown method", func() map[string]any { m := submitInput(); m["method"] = "wallet"; return m }, false},
		{"attempt not integer", func() map[string]any {
			m := submitInput()
			m["metadata"] = map[string]any{"attempt": "two"}
			return m
		}, false},
	}
	for _, tc := range cases {
		errs := c.CheckInput(tc.in())
		if (len(errs) == 0) != tc.ok {
			t.Errorf("%s: ok=%v errs=%v", tc.name, tc.ok, errs)
		}
	}
}

func TestSubmitOutput(t *testing.T) {
	c := mustContract(t, CommercePaymentsSubmit)
	if got := strings.Join(c.Output.Required(), ","); got != "order_ref,status,amount,currency,provider_ref" {
		t.Fatalf("required output = %s: must be the confirm shape", got)
	}
	base := `"order_ref":"SO-1","amount":"1499.00","currency":"MXN","provider_ref":"123"`
	cases := []struct {
		name, out string
		ok        bool
	}{
		{"approved", `{` + base + `,"status":"paid","method":"card","installments":3,"card":{"type":"credit","brand":"master","last4":"0366"}}`, true},
		{"legacy confirm shape", `{` + base + `,"status":"paid"}`, true},
		{"declined", `{` + base + `,"status":"failed","status_detail":"cc_rejected_insufficient_amount","decline_code":"insufficient_funds","retryable":true}`, true},
		{"in review", `{` + base + `,"status":"pending","status_detail":"pending_contingency","action":{"type":"none"}}`, true},
		{"3ds challenge", `{` + base + `,"status":"requires_action","action":{"type":"challenge","challenge":{"url":"https://acs.example/c","creq":"abc"}}}`, true},
		{"cash voucher", `{` + base + `,"status":"requires_action","method":"cash","action":{"type":"voucher","voucher":{"payment_point":"oxxo","reference":"9300","voucher_url":"https://mp.example/v/1","barcode":"123","amount":"1499.00","currency":"MXN","expires_at":"2026-10-22T10:00:00Z"}}}`, true},
		{"spei voucher", `{` + base + `,"status":"requires_action","action":{"type":"voucher","voucher":{"payment_point":"spei","clabe":"646180157000000004"}}}`, true},
		{"redirect", `{` + base + `,"status":"requires_action","action":{"type":"redirect","redirect_url":"https://pay.example/x"}}`, true},
		{"x_ extension", `{` + base + `,"status":"paid","x_mp_status_detail":"accredited"}`, true},
		{"unknown action type", `{` + base + `,"status":"requires_action","action":{"type":"popup"}}`, false},
		{"action without type", `{` + base + `,"status":"requires_action","action":{"voucher":{}}}`, false},
		{"unknown decline code", `{` + base + `,"status":"failed","decline_code":"cc_rejected_other_reason"}`, false},
		{"raw status", `{` + base + `,"status":"approved"}`, false},
		{"no provider_ref", `{"order_ref":"SO-1","amount":"1","currency":"MXN","status":"failed"}`, false},
		{"float amount in voucher", `{` + base + `,"status":"requires_action","action":{"type":"voucher","voucher":{"amount":1499}}}`, false},
		{"PAN in card", `{` + base + `,"status":"paid","card":{"number":"4111111111111111"}}`, false},
		{"undeclared field", `{` + base + `,"status":"paid","token":"tok"}`, false},
	}
	for _, tc := range cases {
		errs := c.ValidateOutput([]byte(tc.out))
		if (len(errs) == 0) != tc.ok {
			t.Errorf("%s: ok=%v errs=%v", tc.name, tc.ok, errs)
		}
	}
	for _, code := range DeclineCodes {
		raw := `{` + base + `,"status":"failed","decline_code":"` + code + `"}`
		if errs := c.ValidateOutput([]byte(raw)); len(errs) != 0 {
			t.Errorf("decline_code %s rejected: %v", code, errs)
		}
	}
}

func clientOutput() string {
	return `{
	  "kind":"mercadopago_bricks","sdk_url":"https://sdk.mercadopago.com/js/v2","public_key":"APP_USR-pub","locale":"es-MX","market":"mx","test_mode":true,
	  "options":[
	    {"key":"card","family":"card","redirect":false,"max_installments":12},
	    {"key":"cash","family":"oxxo","redirect":false,"points":[
	      {"id":"stp","payment_method_id":"bank_transfer","label":"STP"},
	      {"id":"oxxo","payment_method_id":"oxxo","payment_point":"oxxo","label":"OXXO","group":"main"},
	      {"id":"7eleven","payment_method_id":"paycash","payment_point":"7eleven","label":"7 Eleven","logo_url":"https://cdn.example/7.svg"},
	      {"id":"extra","payment_method_id":"paycash","payment_point":"extra","label":"Extra","group":"more"}]},
	    {"key":"wallet","redirect":true},
	    {"key":"credits","redirect":true,"label_max":"Hasta 12 pagos sin tarjeta"}],
	  "csp":{"script":["https://sdk.mercadopago.com","https://http2.mlstatic.com"],"frame":["https://*.mercadopago.com"],"connect":["https://api.mercadopago.com"],"img":["https://http2.mlstatic.com"],"form":["https://*.mercadopago.com"]}
	}`
}

func TestClientContract(t *testing.T) {
	c := mustContract(t, CommercePaymentsClient)
	if got := strings.Join(c.Input.Required(), ","); got != "market,currency" {
		t.Fatalf("required input = %s", got)
	}
	if errs := c.CheckInput(map[string]any{"market": "mx", "currency": "MXN", "amount": "1499.00", "locale": "es-MX"}); len(errs) != 0 {
		t.Fatalf("input: %v", errs)
	}
	for name, in := range map[string]map[string]any{
		"no market":       {"currency": "MXN"},
		"no currency":     {"market": "mx"},
		"float amount":    {"market": "mx", "currency": "MXN", "amount": 10.5},
		"bad currency":    {"market": "mx", "currency": "peso"},
		"blank market":    {"market": " ", "currency": "MXN"},
		"decimal comma":   {"market": "mx", "currency": "MXN", "amount": "10,5"},
		"numeric country": {"market": "mx", "currency": "484"},
	} {
		if errs := c.CheckInput(in); len(errs) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
	if errs := c.ValidateOutput([]byte(clientOutput())); len(errs) != 0 {
		t.Fatalf("full client rejected: %v", errs)
	}
	// Cash needs no SDK script and card-less providers are valid.
	if errs := c.ValidateOutput([]byte(`{"kind":"manual","options":[{"key":"cash","redirect":false}]}`)); len(errs) != 0 {
		t.Fatalf("minimal client rejected: %v", errs)
	}
	for name, bad := range map[string]string{
		"no kind":            `{"options":[]}`,
		"no options":         `{"kind":"x"}`,
		"unknown option key": `{"kind":"x","options":[{"key":"crypto","redirect":false}]}`,
		"option no redirect": `{"kind":"x","options":[{"key":"card"}]}`,
		"point no label":     `{"kind":"x","options":[{"key":"cash","redirect":false,"points":[{"id":"a","payment_method_id":"oxxo"}]}]}`,
		"point bad group":    `{"kind":"x","options":[{"key":"cash","redirect":false,"points":[{"id":"a","payment_method_id":"oxxo","label":"O","group":"top"}]}]}`,
		"installments str":   `{"kind":"x","options":[{"key":"card","redirect":false,"max_installments":"12"}]}`,
		"csp not list":       `{"kind":"x","options":[],"csp":{"script":"https://a"}}`,
		"secret leaks":       `{"kind":"x","options":[],"access_token":"APP_USR-secret"}`,
		"client_secret":      `{"kind":"x","options":[],"client_secret":"cs_live"}`,
	} {
		if errs := c.ValidateOutput([]byte(bad)); len(errs) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
	// The contract declares no field that could carry a secret.
	for _, f := range c.Output.Fields {
		l := strings.ToLower(f.Name)
		if strings.Contains(l, "secret") || strings.Contains(l, "token") || strings.Contains(l, "private") {
			t.Errorf("client output field %q may carry a secret", f.Name)
		}
	}
}

func TestSessionFlowEmbedded(t *testing.T) {
	c := mustContract(t, CommercePaymentsSession)
	// Original required shape untouched (also asserted by the payments standard test).
	if got := strings.Join(c.Input.Required(), ","); got != "order_ref,amount,currency,return_url,idempotency_key" {
		t.Fatalf("session required input changed: %s", got)
	}
	in := map[string]any{"order_ref": "SO-1", "amount": "10.00", "currency": "MXN", "return_url": "https://s/x", "idempotency_key": "k"}
	for _, flow := range []string{"redirect", "embedded"} {
		in["flow"] = flow
		if errs := c.CheckInput(in); len(errs) != 0 {
			t.Errorf("flow %s: %v", flow, errs)
		}
	}
	in["flow"] = "popup"
	if errs := c.CheckInput(in); len(errs) == 0 {
		t.Error("unknown flow accepted")
	}
	for _, hint := range []string{"card", "cash", "wallet", "credits", "oxxo"} {
		in["flow"], in["method_hint"] = "embedded", hint
		if errs := c.CheckInput(in); len(errs) != 0 {
			t.Errorf("method_hint %s: %v", hint, errs)
		}
	}
	// An old provider answer (no flow, no client) is still valid.
	if errs := c.ValidateOutput([]byte(`{"session_id":"s","status":"pending","action_type":"embed","redirect_url":"https://pay/x","instructions":{"brick":{"public_key":"k"}}}`)); len(errs) != 0 {
		t.Fatalf("legacy session output: %v", errs)
	}
	embedded := `{"session_id":"s","status":"pending","action_type":"embed","flow":"embedded","redirect_url":"https://pay/fallback","client":` + clientOutput() + `}`
	if errs := c.ValidateOutput([]byte(embedded)); len(errs) != 0 {
		t.Fatalf("embedded session output: %v", errs)
	}
	// client keeps the provider's own public extras, but typed fields are checked.
	ext := `{"session_id":"s","status":"pending","client":{"kind":"mercadopago_bricks","options":[],"preference_id":"p1"}}`
	if errs := c.ValidateOutput([]byte(ext)); len(errs) != 0 {
		t.Fatalf("client extras rejected: %v", errs)
	}
	if errs := c.ValidateOutput([]byte(`{"session_id":"s","status":"pending","flow":"popup"}`)); len(errs) == 0 {
		t.Error("unknown flow output accepted")
	}
	if errs := c.ValidateOutput([]byte(`{"session_id":"s","status":"pending","client":{"kind":"x","options":[{"key":"crypto","redirect":false}]}}`)); len(errs) == 0 {
		t.Error("typed client fields must be validated")
	}
}

func TestNotificationAndHealthContracts(t *testing.T) {
	insp := mustContract(t, CommercePaymentsNotificationInspect)
	if errs := insp.CheckInput(map[string]any{"method": "POST", "query": map[string]any{"data.id": "123"}, "headers": map[string]any{"x-signature": "ts=1,v1=ab"}, "raw_body_b64": "e30="}); len(errs) != 0 {
		t.Fatalf("inspect input: %v", errs)
	}
	if errs := insp.CheckInput(map[string]any{"query": map[string]any{}}); len(errs) == 0 {
		t.Error("inspect without method accepted")
	}
	for _, auth := range NotificationAuths {
		raw := `{"auth":"` + auth + `","auth_reason":"x","resource_kind":"payment","resource_id":"123","event_id":"e1","livemode":false}`
		if errs := insp.ValidateOutput([]byte(raw)); len(errs) != 0 {
			t.Errorf("auth %s: %v", auth, errs)
		}
	}
	for name, bad := range map[string]string{
		"no auth":      `{"resource_id":"1"}`,
		"inbox state":  `{"auth":"pendiente"}`,
		"livemode str": `{"auth":"firma_ok","livemode":"false"}`,
	} {
		if errs := insp.ValidateOutput([]byte(bad)); len(errs) == 0 {
			t.Errorf("inspect %s accepted", name)
		}
	}

	res := mustContract(t, CommercePaymentsNotificationResolve)
	if errs := res.CheckInput(map[string]any{"resource_kind": "payment", "resource_id": "123", "auth": "firma_invalida"}); len(errs) != 0 {
		t.Fatalf("resolve input: %v", errs)
	}
	for name, in := range map[string]map[string]any{
		"no id":    {"resource_kind": "payment"},
		"no kind":  {"resource_id": "1"},
		"bad auth": {"resource_kind": "payment", "resource_id": "1", "auth": "ok"},
	} {
		if errs := res.CheckInput(in); len(errs) == 0 {
			t.Errorf("resolve %s accepted", name)
		}
	}
	// resolve answers exactly what status answers.
	status := mustContract(t, CommercePaymentsStatus)
	if strings.Join(res.Output.Required(), ",") != strings.Join(status.Output.Required(), ",") || len(res.Output.Fields) != len(status.Output.Fields) {
		t.Error("resolve output must be the status output")
	}

	h := mustContract(t, CommercePaymentsHealth)
	if errs := h.CheckInput(map[string]any{}); len(errs) != 0 {
		t.Fatalf("health takes {}: %v", errs)
	}
	ok := `{"checks":[{"key":"credentials","ok":true},{"key":"webhook_secret","ok":false,"level":"critical","message":"configura la clave"}],"test_mode":true}`
	if errs := h.ValidateOutput([]byte(ok)); len(errs) != 0 {
		t.Fatalf("health output: %v", errs)
	}
	for name, bad := range map[string]string{
		"no checks":  `{}`,
		"no ok":      `{"checks":[{"key":"a"}]}`,
		"no key":     `{"checks":[{"ok":true}]}`,
		"bad level":  `{"checks":[{"key":"a","ok":false,"level":"red"}]}`,
		"ok as text": `{"checks":[{"key":"a","ok":"yes"}]}`,
	} {
		if errs := h.ValidateOutput([]byte(bad)); len(errs) == 0 {
			t.Errorf("health %s accepted", name)
		}
	}
}

func TestStatusAlsoCaptured(t *testing.T) {
	for _, key := range []string{CommercePaymentsConfirm, CommercePaymentsStatus, CommercePaymentsNotificationResolve, CommercePaymentsReconcile} {
		c := mustContract(t, key)
		raw := `{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1","also_captured":["2","3"]}`
		if key == CommercePaymentsReconcile {
			raw = `{"payments":[` + raw + `]}`
		}
		if errs := c.ValidateOutput([]byte(raw)); len(errs) != 0 {
			t.Errorf("%s also_captured: %v", key, errs)
		}
	}
	c := mustContract(t, CommercePaymentsStatus)
	if errs := c.ValidateOutput([]byte(`{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1","also_captured":"2"}`)); len(errs) == 0 {
		t.Error("also_captured must be a list")
	}
}

func TestPaymentEventSourcesAppendOnly(t *testing.T) {
	if got := strings.Join(PaymentEventSources[:4], ","); got != "webhook,poll,reconcile,staff" {
		t.Fatalf("original sources reordered: %s", got)
	}
	ev, ok := LookupEvent(CommercePaymentsConfirmed)
	if !ok {
		t.Fatal("event")
	}
	for _, src := range []string{"webhook", "poll", "reconcile", "staff", "submit", "hint", "return"} {
		raw := `{"provider":"mercadopago","source":"` + src + `","result":{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1"}}`
		if errs := ev.ValidatePayload([]byte(raw)); len(errs) != 0 {
			t.Errorf("source %s: %v", src, errs)
		}
	}
	if errs := ev.ValidatePayload([]byte(`{"provider":"p","source":"browser","result":{"order_ref":"O","status":"paid","amount":"1","currency":"MXN","provider_ref":"1"}}`)); len(errs) == 0 {
		t.Error("unknown source accepted")
	}
}

// Selection of a NEW embedded charge follows the normal rules (organization +
// market); everything after it is pinned and never fans out.
func TestEmbeddedSelectionAndPinning(t *testing.T) {
	mp := Provider{Addon: "mercadopago", Countries: []string{"MX"}, Markets: []string{"mx"}}
	stripe := Provider{Addon: "stripe", Default: true}
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	us := Market{Code: "us", Countries: []string{"US"}}

	// The session (flow=embedded) is a One query: market mx picks MP, us the org default.
	sess := mustContract(t, CommercePaymentsSession)
	for _, tc := range []struct {
		m    Market
		want string
	}{{mx, "mercadopago"}, {us, "stripe"}, {Market{}, "stripe"}} {
		got, err := Select(sess, []Provider{stripe, mp}, tc.m)
		if err != nil || len(got) != 1 || got[0].Addon != tc.want {
			t.Errorf("market %q: %v %v, want %s", tc.m.Code, got, err, tc.want)
		}
	}

	var hits []string
	g := &Engine{
		Source: pinSource(stripe, mp),
		Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
			hits = append(hits, call.Provider.Addon+":"+call.Capability)
			switch call.Capability {
			case CommercePaymentsClient:
				return json.RawMessage(clientOutput()), nil
			default:
				return json.RawMessage(`{"order_ref":"SO-1","status":"paid","amount":"1499.00","currency":"MXN","provider_ref":"9"}`), nil
			}
		},
	}
	// Without a named provider no embedded contract answers, even when the
	// organization has a default and a market selection (single provider != guess).
	for _, key := range embeddedKeys {
		in := map[string]any{}
		if key == CommercePaymentsSubmit {
			in = submitInput()
		}
		if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: key, Input: in}); !errors.Is(err, ErrProviderRequired) {
			t.Errorf("%s without provider: %v", key, err)
		}
	}
	if len(hits) != 0 {
		t.Fatalf("nothing may be invoked: %v", hits)
	}

	// client: the cart asks each candidate by name (not a fan-out): one call each.
	for _, p := range []string{"mercadopago", "stripe"} {
		res, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: CommercePaymentsClient, Provider: p,
			Input: map[string]any{"market": "mx", "currency": "MXN"}})
		if err != nil {
			t.Fatalf("client %s: %v", p, err)
		}
		if _, err := res.One(); err != nil {
			t.Fatalf("client %s result: %v", p, err)
		}
	}
	if strings.Join(hits, ",") != "mercadopago:commerce.payments.client,stripe:commerce.payments.client" {
		t.Fatalf("client hits: %v", hits)
	}

	// submit reaches only the session's provider, even when the default differs.
	hits = nil
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: CommercePaymentsSubmit, Provider: "mercadopago", Input: submitInput()}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(hits) != 1 || hits[0] != "mercadopago:commerce.payments.submit" {
		t.Fatalf("submit must reach only the pinned provider: %v", hits)
	}
	// A pinned provider that does not serve the market is refused, not substituted.
	hits = nil
	if _, err := g.Query(context.Background(), Query{Org: org, Market: us, Capability: CommercePaymentsSubmit, Provider: "mercadopago", Input: submitInput()}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("submit to a provider outside the market: %v", err)
	}
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: CommercePaymentsSubmit, Provider: "ghost", Input: submitInput()}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("submit to an uninstalled provider: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("refused pins must not invoke: %v", hits)
	}
}

func TestSubmitIdempotencyContract(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	g := &Engine{
		Source: pinSource(Provider{Addon: "mercadopago", Default: true}),
		Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
			calls.Add(1)
			keys = append(keys, call.IdempotencyKey)
			return json.RawMessage(`{"order_ref":"SO-1","status":"paid","amount":"1499.00","currency":"MXN","provider_ref":"9"}`), nil
		},
	}
	q := func(in map[string]any) error {
		_, err := g.Query(context.Background(), Query{Org: org, Capability: CommercePaymentsSubmit, Provider: "mercadopago", Input: in})
		return err
	}
	// No key / blank key: refused before any provider call (a charge must never go out unkeyed).
	noKey := submitInput()
	delete(noKey, "idempotency_key")
	if err := q(noKey); err == nil {
		t.Fatal("submit without idempotency_key reached the provider")
	}
	blank := submitInput()
	blank["idempotency_key"] = "  "
	if err := q(blank); err == nil {
		t.Fatal("blank idempotency_key reached the provider")
	}
	if calls.Load() != 0 {
		t.Fatalf("unkeyed submit invoked the provider %d times", calls.Load())
	}
	// The key reaches the provider untouched, on every retry of the same attempt.
	for i := 0; i < 2; i++ {
		if err := q(submitInput()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(keys, ",") != "pay-chk1-a1,pay-chk1-a1" {
		t.Fatalf("keys handed to the provider: %v", keys)
	}
	// Documented: one key per attempt (a retry of the same attempt reuses it; a
	// new card is a new attempt, hence a new key).
	f, _ := mustContract(t, CommercePaymentsSubmit).Input.Field("idempotency_key")
	if !f.Required || !strings.Contains(f.Doc, "one per attempt") {
		t.Errorf("idempotency_key doc: %+v", f)
	}
}

func TestEmbeddedContractsRegistered(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Contracts {
		seen[c.Key] = true
	}
	for _, k := range embeddedKeys {
		if !seen[k] {
			t.Errorf("%s not registered", k)
		}
	}
}
