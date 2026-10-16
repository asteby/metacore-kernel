package apiroute

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

func TestHandler_ServesThroughFiber(t *testing.T) {
	d := &recorder{resp: Response{Body: json.RawMessage(`{"handle":"x"}`)}}
	gw := newTestGateway(t, d)
	app := fiber.New()
	NewHandler(gw, func(c fiber.Ctx) (Principal, error) {
		switch c.Get("Authorization") {
		case "Bearer good":
			return tokenPrincipal("catalog:read"), nil
		case "Bearer slow":
			return Principal{}, RateLimited(7)
		case "":
			return Principal{OrgID: testOrg}, nil
		}
		return Principal{}, io.EOF // untyped → 401
	}).Mount(app.Group("/api/gw"))

	do := func(method, target, auth string, hdr map[string]string) (int, map[string]string, string) {
		t.Helper()
		req := httptest.NewRequest(method, target, nil)
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
		return resp.StatusCode, h, string(b)
	}

	status, h, body := do("GET", "/api/gw/storefront/products/llanta%20205?sku=A&sku=B", "Bearer good", nil)
	if status != 200 || body != `{"handle":"x"}` || h["Cache-Control"] != "public, max-age=60, stale-while-revalidate=600" || h["Etag"] == "" {
		t.Fatalf("GET: %d %v %s", status, h, body)
	}
	if d.req.Params["handle"] != "llanta 205" || strings.Join(d.req.Query["sku"], ",") != "A,B" || d.req.Path != "/storefront/products/llanta%20205" {
		t.Fatalf("envelope: params=%v query=%v path=%q", d.req.Params, d.req.Query, d.req.Path)
	}
	if d.req.Headers["authorization"] != "" {
		t.Fatal("authorization must not reach the handler")
	}

	status, _, body = do("GET", "/api/gw/storefront/products/x", "Bearer good", map[string]string{"If-None-Match": h["Etag"]})
	if status != 304 || body != "" {
		t.Fatalf("304: %d %q", status, body)
	}

	status, _, body = do("HEAD", "/api/gw/storefront/products/x", "Bearer good", nil)
	if status != 200 || body != "" {
		t.Fatalf("HEAD: %d %q", status, body)
	}

	status, _, body = do("GET", "/api/gw/storefront/products/x", "", nil)
	if status != 401 || !strings.Contains(body, `"code":"unauthorized"`) {
		t.Fatalf("anonymous: %d %s", status, body)
	}
	status, _, _ = do("GET", "/api/gw/storefront/products/x", "Bearer bad", nil)
	if status != 401 {
		t.Fatalf("bad token: %d", status)
	}
	status, h, _ = do("GET", "/api/gw/storefront/products/x", "Bearer slow", nil)
	if status != 429 || h["Retry-After"] != "7" {
		t.Fatalf("rate limited: %d %v", status, h)
	}
	status, _, body = do("GET", "/api/gw/nope", "Bearer good", nil)
	if status != 404 || !strings.Contains(body, `"not_found"`) {
		t.Fatalf("404: %d %s", status, body)
	}
	status, h, _ = do("PATCH", "/api/gw/storefront/carts", "Bearer good", nil)
	if status != 405 || h["Allow"] != "POST" {
		t.Fatalf("405: %d %v", status, h)
	}

	// Non-JSON content types are written verbatim.
	d.resp = Response{Headers: map[string]string{"Content-Type": "application/xml"}, Body: json.RawMessage(`"<urlset/>"`)}
	status, h, body = do("GET", "/api/gw/sitemap.xml", "", nil)
	if status != 200 || body != "<urlset/>" || !strings.HasPrefix(h["Content-Type"], "application/xml") {
		t.Fatalf("xml: %d %v %q", status, h, body)
	}
}

func TestHandler_DomainHeaderAndHost(t *testing.T) {
	d := &recorder{}
	shopOrg := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	gw := newTestGateway(t, d, WithDomainResolver(DomainResolverFunc(func(_ context.Context, domain string) (uuid.UUID, error) {
		if domain == "mitienda.mx" {
			return shopOrg, nil
		}
		return uuid.Nil, nil
	})))
	app := fiber.New()
	NewHandler(gw, nil).Mount(app.Group("/api/gw"))

	req := httptest.NewRequest("GET", "http://ops.example/api/gw/sitemap.xml", nil)
	req.Header.Set("X-Storefront-Domain", "mitienda.mx")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != 200 || d.org != shopOrg {
		t.Fatalf("header domain: %v %v %s", err, resp.StatusCode, d.org)
	}

	req = httptest.NewRequest("GET", "http://mitienda.mx:8080/api/gw/sitemap.xml", nil)
	if resp, err = app.Test(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("host domain: %v %v", err, resp.StatusCode)
	}

	req = httptest.NewRequest("GET", "http://ops.example/api/gw/sitemap.xml", nil)
	if resp, err = app.Test(req); err != nil || resp.StatusCode != 404 {
		t.Fatalf("unknown domain: %v %v", err, resp.StatusCode)
	}
}
