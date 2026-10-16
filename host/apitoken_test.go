package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/apitoken"
	"github.com/asteby/metacore-kernel/runtime/apiroute"
)

// TestApp_APITokensAndGateway checks the opt-in wiring: EnableAPITokens
// builds app.APITokens over its table, and APIGateway is mounted at /gw with
// Bearer API tokens resolved into the gateway principal.
func TestApp_APITokensAndGateway(t *testing.T) {
	router := apiroute.NewRouter()
	if err := router.Register("shop", []apiroute.Route{{
		Key: "product", Method: "GET", Path: "/products/:handle", Auth: apiroute.AuthToken,
		Scopes: []string{"catalog:read"}, Cache: &apiroute.Cache{MaxAge: 60}, Do: "wasm:product",
	}}); err != nil {
		t.Fatal(err)
	}
	gw := apiroute.NewGateway(router, apiroute.DispatcherFunc(func(_ context.Context, org uuid.UUID, _ string, req apiroute.Request) (apiroute.Response, error) {
		body, _ := json.Marshal(map[string]string{"handle": req.Params["handle"], "org": org.String()})
		return apiroute.Response{Body: body}, nil
	}))

	app := NewApp(AppConfig{
		DB:              setupHostTestDB(t),
		JWTSecret:       []byte("test-secret-32-bytes-long-xxxxxx"),
		EnableAPITokens: true,
		APIGateway:      gw,
	})
	if app.APITokens == nil {
		t.Fatal("app.APITokens must be built when EnableAPITokens")
	}
	org := uuid.New()
	plain, _, err := app.APITokens.Issue(context.Background(), org, "ssr", apitoken.KindSecret, []string{"catalog:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	f := fiber.New()
	app.Mount(f.Group("/api"))

	req := httptest.NewRequest("GET", "/api/gw/products/llanta", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := f.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("gateway: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	var out map[string]string
	if err := json.Unmarshal(body, &out); err != nil || out["handle"] != "llanta" || out["org"] != org.String() {
		t.Fatalf("body: %s", body)
	}

	resp, _ = f.Test(httptest.NewRequest("GET", "/api/gw/products/llanta", nil))
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	// Staff routes stay behind the JWT middleware.
	resp, _ = f.Test(httptest.NewRequest("GET", "/api/api-tokens", nil))
	if resp.StatusCode != 401 {
		t.Fatalf("staff routes without JWT: %d", resp.StatusCode)
	}

	plainApp := NewApp(AppConfig{DB: setupHostTestDB(t), JWTSecret: []byte("test-secret-32-bytes-long-xxxxxx")})
	if plainApp.APITokens != nil {
		t.Fatal("app.APITokens must stay nil unless EnableAPITokens")
	}
}
