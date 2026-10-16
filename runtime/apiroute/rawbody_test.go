package apiroute

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
)

func TestGateway_RawBodyByteExactAndUnescaped(t *testing.T) {
	const pretty = "{\n  \"note\": \"<b>a&b</b>\",\n  \"n\": 1.50\n}\n"
	for _, body := range []string{pretty, "a=1&b=<2>", "   "} {
		d := &recorder{resp: Response{Body: json.RawMessage(`{}`)}}
		gw := newTestGateway(t, d)
		in := Inbound{Method: "POST", Path: "/storefront/carts", Headers: http.Header{}, Body: []byte(body), Principal: tokenPrincipal("cart:write")}
		if _, err := gw.Handle(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		wire, err := d.req.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			RawBody string `json:"raw_body_b64"`
		}
		if err := json.Unmarshal(wire, &back); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(back.RawBody)
		if err != nil || string(raw) != body {
			t.Fatalf("raw body %q (err %v), want %q", raw, err, body)
		}
		if bytes.Contains(wire, []byte(`\u003c`)) || bytes.Contains(wire, []byte(`\u0026`)) {
			t.Fatalf("Encode HTML-escaped the envelope: %s", wire)
		}
		if bytes.HasSuffix(wire, []byte("\n")) {
			t.Fatal("Encode left a trailing newline")
		}
	}
}

func TestGateway_NoBodyNoRawBody(t *testing.T) {
	d := &recorder{resp: Response{Body: json.RawMessage(`{}`)}}
	gw := newTestGateway(t, d)
	if _, err := gw.Handle(context.Background(), get("/storefront/products/x", tokenPrincipal("catalog:read"))); err != nil {
		t.Fatal(err)
	}
	if d.req.RawBody != "" || d.req.Body != nil {
		t.Fatalf("raw=%q body=%s", d.req.RawBody, d.req.Body)
	}
	wire, _ := d.req.Encode()
	if bytes.Contains(wire, []byte("raw_body_b64")) {
		t.Fatalf("empty raw body serialized: %s", wire)
	}
}
