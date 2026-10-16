package apitoken

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/runtime/apiroute"
)

func TestMemoryLimiter(t *testing.T) {
	clk := newClock()
	l := NewMemoryLimiter(clk.Now)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k", 3); !ok {
			t.Fatalf("request %d denied", i)
		}
	}
	ok, wait := l.Allow("k", 3)
	if ok || wait <= 0 || wait > 20*time.Second {
		t.Fatalf("4th request: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("other", 3); !ok {
		t.Fatal("keys must be independent")
	}
	clk.Advance(20 * time.Second) // 3/min → one token per 20s
	if ok, _ := l.Allow("k", 3); !ok {
		t.Fatal("bucket did not refill")
	}
	if ok, _ := l.Allow("k", 0); !ok {
		t.Fatal("0 = unlimited")
	}
}

func gatewayApp(t *testing.T, svc *Service, cfg ResolverConfig) (*fiber.App, *apiroute.Principal) {
	t.Helper()
	got := &apiroute.Principal{}
	r := apiroute.NewRouter()
	if err := r.Register("shop", []apiroute.Route{
		{Key: "p", Method: "GET", Path: "/products/:h", Auth: apiroute.AuthToken, Scopes: []string{"catalog:read"}, Do: "wasm:p"},
		{Key: "checkout", Method: "POST", Path: "/checkout", Auth: apiroute.AuthToken, Scopes: []string{"checkout:write"}, Do: "wasm:c"},
	}); err != nil {
		t.Fatal(err)
	}
	gw := apiroute.NewGateway(r, apiroute.DispatcherFunc(func(_ context.Context, orgID uuid.UUID, _ string, req apiroute.Request) (apiroute.Response, error) {
		*got = apiroute.Principal{OrgID: orgID, Kind: req.Auth.Kind, Scopes: req.Auth.Scopes, TokenID: uuid.MustParse(req.Auth.TokenID)}
		return apiroute.Response{Body: json.RawMessage(`{}`)}, nil
	}), apiroute.WithDomainResolver(apiroute.DomainResolverFunc(func(_ context.Context, d string) (uuid.UUID, error) {
		if d == "shop.example" {
			return org, nil
		}
		return uuid.Nil, nil
	})))
	app := fiber.New()
	apiroute.NewHandler(gw, NewPrincipalResolver(svc, cfg)).Mount(app.Group("/gw"))
	return app, got
}

func call(t *testing.T, app *fiber.App, method, path, auth string, hdr map[string]string) (int, string, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	h := map[string]string{}
	for k := range resp.Header {
		h[k] = resp.Header.Get(k)
	}
	return resp.StatusCode, string(b), h
}

func TestPrincipalResolver_ThroughGateway(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	svc := newSvc(NewMemoryStore(), clk)
	pk, pkTok, _ := svc.Issue(ctx, org, "browser", KindPublishable, []string{"catalog:read"}, nil, WithRateLimit(2))
	sk, _, _ := svc.Issue(ctx, org, "ssr", KindSecret, []string{"catalog:read", "checkout:write"}, nil)
	pt, ptTok, _ := svc.Issue(ctx, uuid.Nil, "renderer", KindPlatform, []string{"*"}, nil)
	app, got := gatewayApp(t, svc, ResolverConfig{Limiter: NewMemoryLimiter(clk.Now)})

	status, body, _ := call(t, app, "GET", "/gw/products/x", "Bearer "+pk, nil)
	if status != 200 || got.OrgID != org || got.TokenID != pkTok.ID || got.Kind != apiroute.AuthToken {
		t.Fatalf("pk: %d %s %+v", status, body, got)
	}
	// Publishable lacks checkout:write.
	if status, _, _ := call(t, app, "POST", "/gw/checkout", "bearer "+pk, nil); status != 403 {
		t.Fatalf("pk checkout: %d", status)
	}
	// Rate limit (2/min): third request is 429 with Retry-After.
	status, body, h := call(t, app, "GET", "/gw/products/x", "Bearer "+pk, nil)
	if status != 429 || h["Retry-After"] == "" || !strings.Contains(body, "rate_limited") {
		t.Fatalf("rate limit: %d %s %v", status, body, h)
	}
	if status, _, _ := call(t, app, "POST", "/gw/checkout", "Bearer "+sk, nil); status != 200 {
		t.Fatalf("sk checkout: %d", status)
	}
	// Invalid / revoked tokens → 401.
	if status, _, _ := call(t, app, "GET", "/gw/products/x", "Bearer sk_nope", nil); status != 401 {
		t.Fatalf("bad token: %d", status)
	}
	// No token → anonymous → 401 on token routes.
	if status, _, _ := call(t, app, "GET", "/gw/products/x", "", map[string]string{"X-Storefront-Domain": "shop.example"}); status != 401 {
		t.Fatalf("anonymous: %d", status)
	}
	// Platform token: org from the domain header; unknown domain → 404.
	status, _, _ = call(t, app, "GET", "/gw/products/x", "Bearer "+pt, map[string]string{"X-Storefront-Domain": "shop.example"})
	if status != 200 || got.OrgID != org || got.TokenID != ptTok.ID {
		t.Fatalf("platform: %d %+v", status, got)
	}
	status, body, _ = call(t, app, "GET", "/gw/products/x", "Bearer "+pt, map[string]string{"X-Storefront-Domain": "nope.example"})
	if status != 404 || !strings.Contains(body, "organization_not_found") {
		t.Fatalf("platform unknown domain: %d %s", status, body)
	}
	if err := svc.Revoke(ctx, ptTok.ID); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := call(t, app, "GET", "/gw/products/x", "Bearer "+pt, map[string]string{"X-Storefront-Domain": "shop.example"}); status != 401 {
		t.Fatalf("revoked platform: %d", status)
	}
}

func TestPrincipalResolver_Fallback(t *testing.T) {
	svc := newSvc(NewMemoryStore(), newClock())
	called := false
	resolve := NewPrincipalResolver(svc, ResolverConfig{Fallback: func(c fiber.Ctx) (apiroute.Principal, error) {
		called = true
		return apiroute.Principal{OrgID: org, Kind: apiroute.AuthStaff}, nil
	}})
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		p, err := resolve(c)
		if err != nil {
			return err
		}
		return c.SendString(string(p.Kind))
	})
	_, body, _ := call(t, app, "GET", "/", "Bearer eyJhbGciOi.staff.jwt", nil)
	if !called || body != "staff" {
		t.Fatalf("fallback not used: %v %q", called, body)
	}
}

func TestMiddleware(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(NewMemoryStore(), newClock())
	sk, tok, _ := svc.Issue(ctx, org, "ssr", KindSecret, []string{"orders:read"}, nil)
	app := fiber.New()
	app.Get("/x", Middleware(svc, ResolverConfig{}), func(c fiber.Ctx) error {
		p, ok := PrincipalFrom(c)
		if !ok || p.TokenID != tok.ID || p.OrgID != org {
			return c.SendStatus(500)
		}
		return c.SendString("ok")
	})
	if status, body, _ := call(t, app, "GET", "/x", "Bearer "+sk, nil); status != 200 || body != "ok" {
		t.Fatalf("valid: %d %s", status, body)
	}
	if status, _, _ := call(t, app, "GET", "/x", "", nil); status != 401 {
		t.Fatalf("missing: %d", status)
	}
}

func TestStaffHandler(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(NewMemoryStore(), newClock())
	staffUser := uuid.New()
	otherOrg := uuid.New()
	_, foreign, _ := svc.Issue(ctx, otherOrg, "theirs", KindSecret, []string{"a:read"}, nil)

	app := fiber.New()
	NewHandler(svc, func(c fiber.Ctx) (uuid.UUID, uuid.UUID, error) { return org, staffUser, nil }).Mount(app.Group("/api-tokens"))

	send := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	status, out := send("POST", "/api-tokens", `{"name":"SSR","kind":"secret","scopes":["catalog:read"],"rate_limit_per_minute":60}`)
	if status != 201 {
		t.Fatalf("issue: %d %v", status, out)
	}
	data := out["data"].(map[string]any)
	plain := data["token"].(string)
	rec := data["api_token"].(map[string]any)
	if !strings.HasPrefix(plain, "sk_") || rec["created_by"] != staffUser.String() || rec["token_hash"] != nil {
		t.Fatalf("issue payload: %v", data)
	}
	id := rec["id"].(string)

	if status, _ := send("POST", "/api-tokens", `{"name":"p","kind":"platform","scopes":["*"]}`); status != 403 {
		t.Fatalf("platform via staff: %d", status)
	}
	if status, _ := send("POST", "/api-tokens", `{"name":"b","kind":"publishable","scopes":["checkout:write"]}`); status != 400 {
		t.Fatalf("bad publishable: %d", status)
	}

	status, out = send("GET", "/api-tokens", "")
	if status != 200 || len(out["data"].([]any)) != 1 {
		t.Fatalf("list: %d %v", status, out)
	}

	status, out = send("POST", "/api-tokens/"+id+"/rotate", `{"grace_seconds":60}`)
	if status != 200 {
		t.Fatalf("rotate: %d %v", status, out)
	}
	newPlain := out["data"].(map[string]any)["token"].(string)
	if _, err := svc.Authenticate(ctx, plain); err != nil {
		t.Fatalf("old token during grace: %v", err)
	}

	if status, _ := send("POST", "/api-tokens/"+foreign.ID.String()+"/revoke", ""); status != 404 {
		t.Fatalf("foreign revoke: %d", status)
	}
	if status, _ := send("DELETE", "/api-tokens/"+id, ""); status != 200 {
		t.Fatalf("revoke: %d", status)
	}
	if _, err := svc.Authenticate(ctx, plain); err == nil {
		t.Fatal("revoked token still works")
	}
	if _, err := svc.Authenticate(ctx, newPlain); err != nil {
		t.Fatalf("rotated token: %v", err)
	}
}
