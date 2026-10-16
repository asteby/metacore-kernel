package webhookin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// resultDispatcher is a ResultDispatcher returning a canned output.
type resultDispatcher struct {
	captureDispatcher
	out []byte
	err error
}

func (d *resultDispatcher) DispatchResult(ctx context.Context, org uuid.UUID, target string, payload map[string]any) ([]byte, error) {
	_ = d.Dispatch(ctx, org, target, payload)
	return d.out, d.err
}

const prettyBody = "{\n  \"a\": \"<x>&\",\n  \"n\": 1.50\n}\n"

func addonRoute() Route {
	return Route{Key: "mp", Path: "/mp/payments", Verify: VerifyAddon, Do: "wasm:handle_mp"}
}

// roundTrip serializes the payload the way a WASM backend would and decodes
// raw_body_b64 back: the exact bytes must survive.
func roundTrip(t *testing.T, payload map[string]any, enc func(map[string]any) ([]byte, error)) []byte {
	t.Helper()
	wire, err := enc(payload)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RawBodyB64 string `json:"raw_body_b64"`
	}
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	b, err := base64.StdEncoding.DecodeString(got.RawBodyB64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDispatchRequestRawBodyByteExact(t *testing.T) {
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, nil)
	_ = r.Register(addonRoute())
	if _, err := r.DispatchRequest(context.Background(), uuid.New(), Inbound{Path: "/mp/payments", Body: []byte(prettyBody)}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	marshal := func(p map[string]any) ([]byte, error) { return json.Marshal(p) }
	for name, enc := range map[string]func(map[string]any) ([]byte, error){"json.Marshal": marshal, "EncodePayload": EncodePayload} {
		if got := roundTrip(t, disp.payload, enc); string(got) != prettyBody {
			t.Fatalf("%s: raw body not byte-exact: %q", name, got)
		}
	}
	// EncodePayload keeps <>& literal in the convenience body.
	wire, _ := EncodePayload(disp.payload)
	if !bytes.Contains(wire, []byte(`"<x>&"`)) || bytes.Contains(wire, []byte(`\u003c`)) {
		t.Fatalf("EncodePayload escaped HTML: %s", wire)
	}
	// Non-JSON bodies are exact too (form-encoded IPN, empty body).
	for _, body := range [][]byte{[]byte("a=1&b=<2>"), nil} {
		_, _ = r.DispatchRequest(context.Background(), uuid.New(), Inbound{Path: "/mp/payments", Body: body})
		if got := roundTrip(t, disp.payload, EncodePayload); !bytes.Equal(got, body) {
			t.Fatalf("raw body %q != %q", got, body)
		}
	}
}

func TestDispatchRequestQueryMethodHeaders(t *testing.T) {
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, nil)
	route := addonRoute()
	route.Headers = []string{"x-extra-sig", "x-vendor-*"}
	_ = r.Register(route)
	fixed := time.Date(2026, 10, 16, 12, 0, 0, 5, time.UTC)
	now = func() time.Time { return fixed }
	defer func() { now = time.Now }()

	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Signature", "ts=1,v1=abc")
	h.Set("X-Request-Id", "req-1")
	h.Set("Stripe-Signature", "t=1,v1=x")
	h.Set("X-Kueski-Signature", "k")
	h.Set("Authorization", "Bearer tok")
	h.Set("Cookie", "session=1")
	h.Set("X-Internal-Secret", "nope")
	h.Set("X-Extra-Sig", "e")
	h.Set("X-Vendor-Id", "v")
	h.Add("Webhook-Id", "w1")
	h.Add("Webhook-Id", "w2")
	q := url.Values{"data.id": {"123"}, "type": {"payment"}, "multi": {"a", "b"}}

	_, err := r.DispatchRequest(context.Background(), uuid.Nil, Inbound{Path: "/mp/payments", Method: "post", Query: q, Headers: h, Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	p := disp.payload
	if p[PayloadMethod] != "POST" {
		t.Fatalf("method = %v", p[PayloadMethod])
	}
	gotQ := p[PayloadQuery].(map[string][]string)
	if gotQ["data.id"][0] != "123" || len(gotQ["multi"]) != 2 {
		t.Fatalf("query = %v", gotQ)
	}
	q.Set("data.id", "mutated")
	if gotQ["data.id"][0] != "123" {
		t.Fatal("query not copied")
	}
	hdr := p[PayloadHeaders].(map[string]string)
	want := map[string]string{
		"content-type":       "application/json",
		"x-signature":        "ts=1,v1=abc",
		"x-request-id":       "req-1",
		"stripe-signature":   "t=1,v1=x",
		"x-kueski-signature": "k",
		"authorization":      "Bearer tok",
		"x-extra-sig":        "e",
		"x-vendor-id":        "v",
		"webhook-id":         "w1, w2",
	}
	if len(hdr) != len(want) {
		t.Fatalf("headers = %v", hdr)
	}
	for k, v := range want {
		if hdr[k] != v {
			t.Fatalf("header %s = %q, want %q (all: %v)", k, hdr[k], v, hdr)
		}
	}
	if p[PayloadReceivedAt] != "2026-10-16T12:00:00.000000005Z" {
		t.Fatalf("received_at = %v", p[PayloadReceivedAt])
	}
	if p[PayloadOrg] != uuid.Nil.String() || p[PayloadWebhook] != "mp" || p[PayloadPath] != "/mp/payments" {
		t.Fatalf("v0 fields changed: %v", p)
	}
}

func TestFilterHeadersAuthorizationOnlyWhenNotKernelVerified(t *testing.T) {
	h := http.Header{"Authorization": {"Token s3cret"}, "Proxy-Authorization": {"x"}}
	for verify, want := range map[string]bool{VerifyNone: true, VerifyAddon: true, VerifyBearer: false, VerifyHMACSHA256: false} {
		route := Route{Verify: verify, Headers: []string{"authorization", "proxy-authorization", "*"}}
		got := FilterHeaders(route, h)
		if _, ok := got["authorization"]; ok != want {
			t.Fatalf("verify=%q authorization forwarded=%v", verify, ok)
		}
		if _, ok := got["proxy-authorization"]; ok {
			t.Fatal("proxy-authorization must never be forwarded")
		}
	}
}

func TestDispatchRequestAddonVerifyNeedsNoSecret(t *testing.T) {
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, nil) // no SecretResolver
	_ = r.Register(addonRoute())
	if _, err := r.DispatchRequest(context.Background(), uuid.New(), Inbound{Path: "/mp/payments", Body: []byte("x")}); err != nil {
		t.Fatalf("addon verify should skip kernel verification: %v", err)
	}
	if disp.calls != 1 {
		t.Fatal("not dispatched")
	}
}

func TestDispatchRequestResponseEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		envelope bool
		status   int
		body     string
		headers  map[string]string
	}{
		{"aplazo ack", `{"status":200,"body":{"status":true}}`, true, 200, `{"status":true}`, map[string]string{}},
		{"jwt text", `{"status":200,"headers":{"content-type":"text/plain","Set-Cookie":"x","Retry-After":"a\r\nb"},"body":"eyJ.a.b"}`, true, 200, "eyJ.a.b", map[string]string{"Content-Type": "text/plain"}},
		{"guest 401", `{"status":401}`, true, 401, "", map[string]string{}},
		{"guest result envelope", `{"success":true,"data":{}}`, false, 0, "", nil},
		{"unknown key", `{"status":200,"data":1}`, false, 0, "", nil},
		{"no status", `{"body":{}}`, false, 0, "", nil},
		{"bad status", `{"status":42}`, false, 0, "", nil},
		{"trailing", `{"status":200} {}`, false, 0, "", nil},
		{"empty", ``, false, 0, "", nil},
		{"array", `[1]`, false, 0, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &resultDispatcher{out: []byte(tc.out)}
			r := New(map[string]Dispatcher{"wasm": disp}, nil)
			_ = r.Register(addonRoute())
			resp, err := r.DispatchRequest(context.Background(), uuid.New(), Inbound{Path: "/mp/payments"})
			if err != nil {
				t.Fatal(err)
			}
			if string(resp.Output) != tc.out {
				t.Fatalf("output = %q", resp.Output)
			}
			if resp.Envelope != tc.envelope {
				t.Fatalf("envelope = %v", resp.Envelope)
			}
			if !tc.envelope {
				return
			}
			if resp.Status != tc.status || string(resp.BodyBytes()) != tc.body {
				t.Fatalf("status=%d body=%q", resp.Status, resp.BodyBytes())
			}
			if len(resp.Headers) != len(tc.headers) {
				t.Fatalf("headers = %v", resp.Headers)
			}
			for k, v := range tc.headers {
				if resp.Headers[k] != v {
					t.Fatalf("headers = %v", resp.Headers)
				}
			}
		})
	}
}

func TestDispatchRequestResultError(t *testing.T) {
	boom := errors.New("boom")
	disp := &resultDispatcher{err: boom}
	r := New(map[string]Dispatcher{"wasm": disp}, nil)
	_ = r.Register(addonRoute())
	if _, err := r.DispatchRequest(context.Background(), uuid.New(), Inbound{Path: "/mp/payments"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestDispatchBackCompatUsesRequestPath(t *testing.T) {
	secret := "shh"
	body := []byte(prettyBody)
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: secret})
	_ = r.Register(githubRoute())
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+sign(secret, body))
	h.Set("Authorization", "Bearer must-not-leak")
	if err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/github", h, body); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	p := disp.payload
	if p[PayloadMethod] != "POST" || len(p[PayloadQuery].(map[string][]string)) != 0 {
		t.Fatalf("defaults: %v %v", p[PayloadMethod], p[PayloadQuery])
	}
	if _, ok := p[PayloadHeaders].(map[string]string)["authorization"]; ok {
		t.Fatal("authorization forwarded on a kernel-verified route")
	}
	if b, ok := p[PayloadBody].(json.RawMessage); !ok || string(b) != prettyBody {
		t.Fatalf("body kept for back-compat: %#v", p[PayloadBody])
	}
	if got := roundTrip(t, p, EncodePayload); string(got) != prettyBody {
		t.Fatalf("raw = %q", got)
	}
	// A wrong signature still fails before dispatch.
	h.Set("X-Hub-Signature-256", "sha256="+sign("other", body))
	if _, err := r.DispatchRequest(context.Background(), uuid.New(), Inbound{Path: "/webhooks/github", Headers: h, Body: body}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v", err)
	}
}
