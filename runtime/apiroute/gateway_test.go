package apiroute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// recorder is a Dispatcher that records the last request and answers with a
// canned response or error.
type recorder struct {
	calls  int
	org    uuid.UUID
	target string
	req    Request
	resp   Response
	err    error
}

func (r *recorder) Invoke(_ context.Context, org uuid.UUID, target string, req Request) (Response, error) {
	r.calls++
	r.org, r.target, r.req = org, target, req
	return r.resp, r.err
}

var testOrg = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func tokenPrincipal(scopes ...string) Principal {
	return Principal{OrgID: testOrg, Kind: AuthToken, Scopes: scopes, TokenID: uuid.MustParse("22222222-2222-2222-2222-222222222222")}
}

func newTestGateway(t *testing.T, d Dispatcher, opts ...Option) *Gateway {
	t.Helper()
	r := NewRouter()
	mustRegister(t, r, "storefront_api",
		Route{Key: "product_get", Method: "GET", Path: "/storefront/products/:handle", Auth: AuthToken,
			Scopes: []string{"catalog:read"}, Cache: &Cache{MaxAge: 60, SWR: 600}, Do: "wasm:product_get"},
		Route{Key: "cart_get", Method: "GET", Path: "/storefront/carts/:id", Auth: AuthToken,
			Scopes: []string{"cart:write"}, Cache: &Cache{Private: true}, Do: "wasm:cart_get"},
		Route{Key: "cart_create", Method: "POST", Path: "/storefront/carts", Auth: AuthToken,
			Scopes: []string{"cart:write"}, Do: "wasm:cart_create"},
		Route{Key: "me", Method: "GET", Path: "/storefront/customer", Auth: AuthCustomer,
			Scopes: []string{"customer:read"}, Do: "wasm:me"},
		Route{Key: "admin", Method: "GET", Path: "/admin/stats", Auth: AuthStaff, Do: "wasm:admin"},
		Route{Key: "sitemap", Method: "GET", Path: "/sitemap.xml", Auth: AuthNone, Do: "wasm:sitemap"},
	)
	return NewGateway(r, d, opts...)
}

func get(path string, p Principal) Inbound {
	return Inbound{Method: "GET", Path: path, Headers: http.Header{}, Principal: p}
}

func TestGateway_BuildsRequestEnvelope(t *testing.T) {
	d := &recorder{resp: Response{Body: json.RawMessage(`{"ok":true}`)}}
	gw := newTestGateway(t, d)
	in := Inbound{
		Method: "POST",
		Path:   "/storefront/carts",
		Query:  map[string][]string{"sku": {"A", "B"}},
		Headers: http.Header{
			"Accept":          {"application/json"},
			"Authorization":   {"Bearer sk_secret"},
			"Cookie":          {"session=1"},
			"X-Request-Id":    {"req-1"},
			"Accept-Language": {"es-MX"},
		},
		Body:      []byte(`{"lines":[]}`),
		Principal: tokenPrincipal("cart:write"),
	}
	if _, err := gw.Handle(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	req := d.req
	if d.org != testOrg || d.target != "wasm:cart_create" || req.Addon != "storefront_api" || req.Route != "cart_create" {
		t.Fatalf("dispatch target wrong: org=%s target=%s req=%+v", d.org, d.target, req)
	}
	if req.Headers["authorization"] != "" || req.Headers["cookie"] != "" {
		t.Fatalf("non-allowlisted headers leaked: %v", req.Headers)
	}
	if req.Headers["accept"] != "application/json" || req.Headers["x-request-id"] != "req-1" || req.Headers["accept-language"] != "es-MX" {
		t.Fatalf("allowlisted headers missing: %v", req.Headers)
	}
	if string(req.Body) != `{"lines":[]}` || strings.Join(req.Query["sku"], ",") != "A,B" {
		t.Fatalf("body/query wrong: %s %v", req.Body, req.Query)
	}
	if req.Auth.Kind != AuthToken || req.Auth.TokenID == "" || req.Auth.Scopes[0] != "cart:write" {
		t.Fatalf("auth wrong: %+v", req.Auth)
	}
	raw, _ := json.Marshal(req)
	for _, want := range []string{`"org":"11111111-1111-1111-1111-111111111111"`, `"params":{}`, `"method":"POST"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("envelope %s missing %s", raw, want)
		}
	}

	// Non-JSON body travels as a JSON string.
	in.Body = []byte("a=1&b=2")
	if _, err := gw.Handle(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	var form string
	if err := json.Unmarshal(d.req.Body, &form); err != nil || form != "a=1&b=2" {
		t.Fatalf("form body = %s", d.req.Body)
	}
}

func TestGateway_ExtraRequestHeaders(t *testing.T) {
	d := &recorder{}
	gw := newTestGateway(t, d, WithRequestHeaders("Idempotency-Key"))
	in := get("/storefront/products/x", tokenPrincipal("catalog:read"))
	in.Headers.Set("Idempotency-Key", "k1")
	if _, err := gw.Handle(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if d.req.Headers["idempotency-key"] != "k1" || d.req.Params["handle"] != "x" {
		t.Fatalf("headers = %v params = %v", d.req.Headers, d.req.Params)
	}
}

func TestGateway_AuthAndScopes(t *testing.T) {
	d := &recorder{}
	gw := newTestGateway(t, d)
	ctx := context.Background()
	cases := []struct {
		name string
		in   Inbound
		want error
	}{
		{"anonymous on token route", get("/storefront/products/x", Principal{OrgID: testOrg}), ErrUnauthorized},
		{"no org", get("/storefront/products/x", Principal{Kind: AuthToken, Scopes: []string{"catalog:read"}}), ErrUnauthorized},
		{"no org on public route", get("/sitemap.xml", Principal{}), ErrNotFound},
		{"missing scope", get("/storefront/products/x", tokenPrincipal("cart:write")), ErrForbidden},
		{"token on customer route", get("/storefront/customer", tokenPrincipal("*")), ErrUnauthorized},
		{"token on staff route", get("/admin/stats", tokenPrincipal("*")), ErrUnauthorized},
		{"customer missing scope", get("/storefront/customer", Principal{OrgID: testOrg, Kind: AuthCustomer}), ErrForbidden},
		{"unknown path", get("/nope", tokenPrincipal("*")), ErrNotFound},
	}
	for _, tc := range cases {
		if _, err := gw.Handle(ctx, tc.in); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if d.calls != 0 {
		t.Fatalf("dispatcher must not run on rejected requests, ran %d", d.calls)
	}
	ok := []struct {
		name string
		in   Inbound
	}{
		{"exact scope", get("/storefront/products/x", tokenPrincipal("catalog:read"))},
		{"area wildcard", get("/storefront/products/x", tokenPrincipal("catalog:*"))},
		{"customer on token route", get("/storefront/products/x", Principal{OrgID: testOrg, Kind: AuthCustomer, Scopes: []string{"catalog:read"}, CustomerID: "c1"})},
		{"customer route", get("/storefront/customer", Principal{OrgID: testOrg, Kind: AuthCustomer, Scopes: []string{"customer:read"}, CustomerID: "c1"})},
		{"staff route", get("/admin/stats", Principal{OrgID: testOrg, Kind: AuthStaff, UserID: uuid.New()})},
		{"public route anonymous", get("/sitemap.xml", Principal{OrgID: testOrg})},
	}
	for _, tc := range ok {
		if _, err := gw.Handle(ctx, tc.in); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if d.req.Auth.Kind != AuthNone {
		t.Fatalf("anonymous kind = %q", d.req.Auth.Kind)
	}
}

func TestGateway_MethodNotAllowed(t *testing.T) {
	gw := newTestGateway(t, &recorder{})
	_, err := gw.Handle(context.Background(), Inbound{Method: "DELETE", Path: "/storefront/carts", Principal: tokenPrincipal("*")})
	if !errors.Is(err, ErrMethodNotAllowed) {
		t.Fatalf("got %v", err)
	}
	resp := ErrorResponse(err)
	if resp.Status != 405 || resp.Headers["Allow"] != "POST" {
		t.Fatalf("405 response wrong: %+v", resp)
	}
}

func TestGateway_BodyLimit(t *testing.T) {
	d := &recorder{}
	gw := newTestGateway(t, d, WithMaxBodyBytes(8))
	in := Inbound{Method: "POST", Path: "/storefront/carts", Body: []byte(`{"lines":[1,2]}`), Principal: tokenPrincipal("cart:write")}
	if _, err := gw.Handle(context.Background(), in); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("got %v", err)
	}
	if NewGateway(nil, d).MaxBodyBytes() != DefaultMaxBodyBytes {
		t.Fatal("default limit must be 1 MiB")
	}
}

func TestGateway_AddonEnabled(t *testing.T) {
	d := &recorder{}
	enabled := false
	gw := newTestGateway(t, d, WithAddonEnabled(func(_ context.Context, org uuid.UUID, addon string) (bool, error) {
		if org != testOrg || addon != "storefront_api" {
			t.Fatalf("enabled called with %s %s", org, addon)
		}
		return enabled, nil
	}))
	in := get("/storefront/products/x", tokenPrincipal("catalog:read"))
	if _, err := gw.Handle(context.Background(), in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled addon: %v", err)
	}
	enabled = true
	if _, err := gw.Handle(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func TestGateway_CacheDerivationAndETag(t *testing.T) {
	d := &recorder{resp: Response{Body: json.RawMessage(`{"handle":"x"}`)}}
	gw := newTestGateway(t, d)
	ctx := context.Background()

	resp, err := gw.Handle(ctx, get("/storefront/products/x", tokenPrincipal("catalog:read")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || resp.Headers["Cache-Control"] != "public, max-age=60, stale-while-revalidate=600" {
		t.Fatalf("derived cache-control wrong: %+v", resp)
	}
	etag := resp.Headers["ETag"]
	if !strings.HasPrefix(etag, `W/"`) || resp.Headers["Content-Type"] != "application/json; charset=utf-8" {
		t.Fatalf("etag/content-type wrong: %+v", resp.Headers)
	}

	// If-None-Match → 304 with no body, keeping cache validators.
	in := get("/storefront/products/x", tokenPrincipal("catalog:read"))
	in.Headers.Set("If-None-Match", `"other", `+strings.TrimPrefix(etag, "W/"))
	resp, err = gw.Handle(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 304 || len(resp.Body) != 0 || resp.Headers["ETag"] != etag || resp.Headers["Content-Type"] != "" ||
		resp.Headers["Cache-Control"] == "" {
		t.Fatalf("304 wrong: %+v", resp)
	}
	in.Headers.Set("If-None-Match", "*")
	if resp, _ = gw.Handle(ctx, in); resp.Status != 304 {
		t.Fatalf("* must match: %d", resp.Status)
	}
	in.Headers.Set("If-None-Match", `W/"stale"`)
	if resp, _ = gw.Handle(ctx, in); resp.Status != 200 {
		t.Fatalf("stale etag must not 304: %d", resp.Status)
	}

	// Private cache policy.
	resp, _ = gw.Handle(ctx, get("/storefront/carts/c1", tokenPrincipal("cart:write")))
	if resp.Headers["Cache-Control"] != "private, no-store" {
		t.Fatalf("private cache-control = %q", resp.Headers["Cache-Control"])
	}

	// Handler-provided Cache-Control and ETag win.
	d.resp = Response{Headers: map[string]string{"cache-control": "public, max-age=5", "etag": `"v1"`}, Body: json.RawMessage(`{}`)}
	resp, _ = gw.Handle(ctx, get("/storefront/products/x", tokenPrincipal("catalog:read")))
	if resp.Headers["Cache-Control"] != "public, max-age=5" || resp.Headers["ETag"] != `"v1"` {
		t.Fatalf("handler headers must win: %+v", resp.Headers)
	}

	// Non-2xx on a cacheable route is not cached.
	d.resp = Response{Status: 404, Body: json.RawMessage(`{"error":{"code":"not_found","message":"x"}}`)}
	resp, _ = gw.Handle(ctx, get("/storefront/products/x", tokenPrincipal("catalog:read")))
	if resp.Status != 404 || resp.Headers["Cache-Control"] != "no-store" || resp.Headers["ETag"] != "" {
		t.Fatalf("404 must not be cached: %+v", resp)
	}

	// Routes without a cache policy default to no-store, POST gets no ETag.
	d.resp = Response{Status: 201, Body: json.RawMessage(`{"id":"c1"}`)}
	resp, _ = gw.Handle(ctx, Inbound{Method: "POST", Path: "/storefront/carts", Principal: tokenPrincipal("cart:write")})
	if resp.Status != 201 || resp.Headers["Cache-Control"] != "no-store" || resp.Headers["ETag"] != "" {
		t.Fatalf("POST response wrong: %+v", resp)
	}
}

func TestGateway_ResponseHeaderAllowlistAndStatus(t *testing.T) {
	d := &recorder{}
	gw := newTestGateway(t, d)
	ctx := context.Background()
	in := Inbound{Method: "POST", Path: "/storefront/carts", Principal: tokenPrincipal("cart:write")}

	d.resp = Response{Status: 201, Headers: map[string]string{
		"Location":                    "/storefront/carts/c1",
		"Set-Cookie":                  "evil=1",
		"Access-Control-Allow-Origin": "*",
		"Vary":                        "Accept-Language",
		"X-Powered-By":                "addon",
		"Retry-After":                 "bad\r\nX-Injected: 1",
	}, Body: json.RawMessage(`{"id":"c1"}`)}
	resp, err := gw.Handle(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Set-Cookie", "Access-Control-Allow-Origin", "X-Powered-By", "Retry-After"} {
		if _, ok := resp.Headers[h]; ok {
			t.Fatalf("header %s must be dropped: %+v", h, resp.Headers)
		}
	}
	if resp.Headers["Location"] != "/storefront/carts/c1" || resp.Headers["Vary"] != "Accept-Language" {
		t.Fatalf("allowlisted headers lost: %+v", resp.Headers)
	}

	// 204 drops the body.
	d.resp = Response{Status: 204, Body: json.RawMessage(`{"x":1}`)}
	if resp, _ = gw.Handle(ctx, in); resp.Status != 204 || resp.Body != nil {
		t.Fatalf("204 wrong: %+v", resp)
	}
	// Out-of-range status is a bad response.
	for _, s := range []int{42, 600, -1} {
		d.resp = Response{Status: s}
		if _, err := gw.Handle(ctx, in); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("status %d: got %v", s, err)
		}
	}
}

func TestGateway_DispatcherErrors(t *testing.T) {
	d := &recorder{err: errors.New("wasm trap: unreachable")}
	gw := newTestGateway(t, d)
	in := Inbound{Method: "POST", Path: "/storefront/carts", Principal: tokenPrincipal("cart:write")}
	_, err := gw.Handle(context.Background(), in)
	if !errors.Is(err, ErrHandlerFailed) {
		t.Fatalf("got %v", err)
	}
	resp := ErrorResponse(err)
	if resp.Status != 502 || strings.Contains(string(resp.Body), "trap") {
		t.Fatalf("internal detail leaked or wrong status: %d %s", resp.Status, resp.Body)
	}

	d.err = &Error{Status: 409, Code: "insufficient_stock", Message: "not enough stock", Field: "lines"}
	_, err = gw.Handle(context.Background(), in)
	resp = ErrorResponse(err)
	if resp.Status != 409 || string(resp.Body) != `{"error":{"code":"insufficient_stock","field":"lines","message":"not enough stock"}}` {
		t.Fatalf("typed handler error wrong: %d %s", resp.Status, resp.Body)
	}

	if _, err := NewGateway(gw.Router(), nil).Handle(context.Background(), in); !errors.Is(err, ErrNoDispatcher) {
		t.Fatalf("nil dispatcher: %v", err)
	}
}

func TestErrorResponse(t *testing.T) {
	resp := ErrorResponse(errors.New("db password is hunter2"))
	if resp.Status != 500 || strings.Contains(string(resp.Body), "hunter2") || resp.Headers["Cache-Control"] != "no-store" {
		t.Fatalf("untyped error: %d %s", resp.Status, resp.Body)
	}
	resp = ErrorResponse(RateLimited(30))
	if resp.Status != 429 || resp.Headers["Retry-After"] != "30" || !errors.Is(RateLimited(30), ErrRateLimited) {
		t.Fatalf("rate limited: %+v", resp)
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(ErrorResponse(ErrUnauthorized).Body, &body); err != nil || body.Error.Code != "unauthorized" {
		t.Fatalf("body = %+v %v", body, err)
	}
}

func TestDecodeResponse(t *testing.T) {
	r, err := DecodeResponse([]byte(`{"status":201,"headers":{"Location":"/x"},"body":{"id":1}}`))
	if err != nil || r.Status != 201 || r.Headers["Location"] != "/x" || string(r.Body) != `{"id":1}` {
		t.Fatalf("decode: %+v %v", r, err)
	}
	if r, err := DecodeResponse(nil); err != nil || r.Status != 204 {
		t.Fatalf("empty: %+v %v", r, err)
	}
	for _, bad := range []string{`{"id":1}`, `not json`, `{"status":200} {}`, `[1]`} {
		if _, err := DecodeResponse([]byte(bad)); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("%s: got %v", bad, err)
		}
	}
}

func TestResponse_BodyBytes(t *testing.T) {
	r := Response{Headers: map[string]string{"Content-Type": "application/xml"}, Body: json.RawMessage(`"<urlset/>"`)}
	if string(r.BodyBytes()) != "<urlset/>" {
		t.Fatalf("xml body = %s", r.BodyBytes())
	}
	r = Response{Body: json.RawMessage(`"plain json string"`)}
	if string(r.BodyBytes()) != `"plain json string"` {
		t.Fatalf("json string body must stay quoted: %s", r.BodyBytes())
	}
	r = Response{Headers: map[string]string{"Content-Type": "application/problem+json"}, Body: json.RawMessage(`"x"`)}
	if string(r.BodyBytes()) != `"x"` {
		t.Fatalf("+json body = %s", r.BodyBytes())
	}
}
