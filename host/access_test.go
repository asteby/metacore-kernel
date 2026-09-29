package host

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
)

var hostSecret = []byte("test-secret-32-bytes-long-xxxxxx")

func hostToken(t *testing.T, org uuid.UUID, email, role string) string {
	t.Helper()
	tok, _, err := auth.GenerateToken(auth.Claims{UserID: uuid.New(), OrganizationID: org, Email: email, Role: role}, hostSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func hostDo(t *testing.T, app *fiber.App, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env
}

// TestApp_AccessPolicyWithRoleResolver drives the full host stack (JWT →
// RoleResolver → dynamic policy): buyers are "owner" of their own org, and
// only the emails the app lists become "store.admin".
func TestApp_AccessPolicyWithRoleResolver(t *testing.T) {
	db := setupHostTestDB(t)
	configured := false
	resolverCalls := 0
	app := NewApp(AppConfig{
		DB:        db,
		JWTSecret: hostSecret,
		RoleResolver: func(c fiber.Ctx) []string {
			resolverCalls++
			if auth.GetEmail(c) == "admin@7leguas.mx" {
				return []string{"store.admin"}
			}
			return nil
		},
		ConfigureDynamic: func(c *dynamic.Config) { configured = c.Metadata != nil },
	})
	if !configured {
		t.Fatal("ConfigureDynamic must receive the built dynamic.Config")
	}
	factory := func() modelbase.ModelDefiner { return &hostTestProduct{} }
	app.RegisterModel("host_policy_products", factory, WithAccess(modelbase.AccessPublicReadStaffWrite("store.admin")))
	app.RegisterModel("host_open_products", factory)
	app.RegisterModel("host_settings", factory, AsSingleton(), WithAccess(modelbase.AccessStaffOnly("store.admin")))

	f := fiber.New()
	app.Mount(f.Group("/api"))

	org := uuid.New()
	buyer := hostToken(t, org, "buyer@example.com", "owner")
	admin := hostToken(t, org, "admin@7leguas.mx", "owner")

	// No policy: the RoleResolver is never consulted.
	if st, env := hostDo(t, f, buyer, "POST", "/api/dynamic/host_open_products", `{"name":"a"}`); st != fiber.StatusCreated {
		t.Fatalf("open create: %d %v", st, env)
	}
	if resolverCalls != 0 {
		t.Fatalf("RoleResolver must be lazy, called %d times", resolverCalls)
	}

	st, env := hostDo(t, f, buyer, "POST", "/api/dynamic/host_policy_products", `{"name":"a"}`)
	if st != fiber.StatusForbidden || env["code"] != "access_denied" {
		t.Fatalf("buyer create: %d %v", st, env)
	}
	if st, _ := hostDo(t, f, buyer, "GET", "/api/dynamic/host_policy_products", ""); st != fiber.StatusOK {
		t.Fatalf("buyer list: %d", st)
	}
	if st, env := hostDo(t, f, admin, "POST", "/api/dynamic/host_policy_products", `{"name":"a"}`); st != fiber.StatusCreated {
		t.Fatalf("admin create: %d %v", st, env)
	}

	adminOrg := uuid.New()
	admin2 := hostToken(t, adminOrg, "admin@7leguas.mx", "owner")
	st, env = hostDo(t, f, admin2, "GET", "/api/dynamic/host_settings/current", "")
	if st != fiber.StatusOK {
		t.Fatalf("settings current: %d %v", st, env)
	}
	if st, env = hostDo(t, f, admin2, "POST", "/api/dynamic/host_settings", `{"name":"b"}`); st != fiber.StatusConflict {
		t.Fatalf("second settings create: %d %v, want 409", st, env)
	}
	if st, _ = hostDo(t, f, hostToken(t, adminOrg, "x@y.z", "owner"), "GET", "/api/dynamic/host_settings/current", ""); st != fiber.StatusForbidden {
		t.Fatalf("buyer settings read: %d, want 403", st)
	}
}
