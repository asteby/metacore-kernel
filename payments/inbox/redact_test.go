package inbox

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const hash64 = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

func TestRedactSignature(t *testing.T) {
	cases := map[string]string{
		"ts=1760997480123,v1=" + hash64:               "ts=1760997480123,v1=[64h:ab12…]",
		"t=1760997480,v1=" + hash64 + ",v1=" + hash64: "t=1760997480,v1=[64h:ab12…],v1=[64h:ab12…]",
		"ts=1,v0=short":                               "ts=1,v0=short",
		"Bearer " + strings.Repeat("Zx", 20):          "[47c:Bear…]",
	}
	for in, want := range cases {
		if got := RedactSignature(in); got != want {
			t.Errorf("RedactSignature(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(RedactSignature(in), hash64) {
			t.Errorf("hash survived in %q", in)
		}
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Signature", "ts=1760997480123,v1="+hash64)
	h.Set("Stripe-Signature", "t=1,v1="+hash64)
	h.Set("Authorization", "Bearer APP_USR-123456789")
	h.Set("Cookie", "session=abc")
	h.Set("X-Request-Id", "req-1")
	h.Set("User-Agent", "MercadoPago")
	got := RedactHeaders(h)
	if _, ok := got["authorization"]; ok {
		t.Error("authorization kept")
	}
	if _, ok := got["cookie"]; ok {
		t.Error("cookie kept")
	}
	if got["x-request-id"] != "req-1" || got["user-agent"] != "MercadoPago" {
		t.Errorf("allowlisted headers lost: %v", got)
	}
	raw, _ := json.Marshal(got)
	for _, bad := range []string{hash64, "APP_USR-", "session=abc"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("%q leaked into %s", bad, raw)
		}
	}
}

func TestRedactBody(t *testing.T) {
	body := `{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1","amount":1500,
	"receipt_email":"a@b.com","email":"buyer@example.com","phone":"5512345678",
	"billing_details":{"name":"Ana","address":{"city":"CDMX"}},
	"payer":{"id":"77","email":"p@x.com"},
	"card":{"last_four_digits":"4242","brand":"visa","number":"4111111111111111","cardholder":{"name":"Ana"}},
	"note":"sig ` + hash64 + ` and sk_live_abcDEF123 token APP_USR-9999-abc"}}}`
	out := string(RedactBody([]byte(body)))
	for _, bad := range []string{"buyer@example.com", "5512345678", "Ana", "CDMX", "p@x.com", "4111111111111111", hash64, "sk_live_abcDEF123", "APP_USR-9999"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q leaked: %s", bad, out)
		}
	}
	for _, keep := range []string{"evt_1", "pi_1", "1500", "4242", "visa"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q should survive: %s", keep, out)
		}
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("not JSON: %s", out)
	}
}

func TestRedactBodyNotJSON(t *testing.T) {
	out := string(RedactBody([]byte("a=1&email=x@y.com")))
	if strings.Contains(out, "x@y.com") || !json.Valid([]byte(out)) {
		t.Errorf("unparsed body not masked: %s", out)
	}
	if string(RedactBody(nil)) != "{}" {
		t.Error("empty body should be {}")
	}
}
