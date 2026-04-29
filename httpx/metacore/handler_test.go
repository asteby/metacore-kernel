package metacore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/asteby/metacore-kernel/bridge"
	kerneltool "github.com/asteby/metacore-kernel/tool"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupTestDB returns an in-memory sqlite DB with the metacore_installations
// table pre-created. installer.Installation declares Postgres-only DDL
// (gen_random_uuid(), jsonb) that the GORM SQLite driver can't translate
// via AutoMigrate, so we materialize a sqlite-compatible table whose
// columns line up with the model's tags. Subsequent AutoMigrate calls in
// host.New() are a no-op (all columns/indexes already present).
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Exec(`CREATE TABLE metacore_installations (
		id text PRIMARY KEY,
		organization_id text NOT NULL,
		addon_key text NOT NULL,
		version text NOT NULL,
		status text NOT NULL DEFAULT "enabled",
		source text NOT NULL,
		secret_hash text,
		secret_enc text,
		settings text,
		installed_at datetime,
		enabled_at datetime,
		disabled_at datetime
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX idx_org_addon ON metacore_installations(organization_id, addon_key)`).Error; err != nil {
		t.Fatalf("create idx: %v", err)
	}
	return db
}

// TestNewHandler_RequiresBridge verifies the constructor refuses to build
// without a bridge — every route depends on it.
func TestNewHandler_RequiresBridge(t *testing.T) {
	if _, err := NewHandler(Deps{}); err == nil {
		t.Fatal("expected error when Bridge is nil")
	}
}

// TestNewHandler_DefaultsRegistry verifies the registry defaults to the
// process-global one when callers don't pass one explicitly.
func TestNewHandler_DefaultsRegistry(t *testing.T) {
	db := setupTestDB(t)
	b, err := bridge.New(bridge.Config{DB: db})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	h, err := NewHandler(Deps{Bridge: b})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if h.deps.ToolRegistry == nil {
		t.Fatal("expected default registry")
	}
	if h.deps.ToolRegistry != kerneltool.GlobalRegistry() {
		t.Fatal("expected default to be GlobalRegistry()")
	}
}

// TestListManifests_AnonymousReturnsEmpty verifies the route returns an
// empty array (not 401) when no organization context is wired into the
// Fiber locals. The SDK frontend bootstrap calls this endpoint before
// auth state has hydrated; returning [] keeps that path clean.
func TestListManifests_AnonymousReturnsEmpty(t *testing.T) {
	db := setupTestDB(t)
	b, err := bridge.New(bridge.Config{DB: db})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	h, err := NewHandler(Deps{Bridge: b})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	app := fiber.New()
	app.Get("/manifests", h.ListManifests)
	req := httptest.NewRequest("GET", "/manifests", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out []interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty array, got %v", out)
	}
}

// TestListManifests_EmptyForNewOrg verifies the route returns an empty
// data slice for an org with zero installations — nothing crashes when
// the kernel has just been booted.
func TestListManifests_EmptyForNewOrg(t *testing.T) {
	db := setupTestDB(t)
	b, err := bridge.New(bridge.Config{DB: db})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	h, err := NewHandler(Deps{Bridge: b})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	app := fiber.New()
	orgID := uuid.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals("organization_id", orgID)
		return c.Next()
	})
	app.Get("/manifests", h.ListManifests)
	req := httptest.NewRequest("GET", "/manifests", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var out []interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty array, got %v", out)
	}
	if v := resp.Header.Get("X-Metacore-Kernel-Version"); v == "" {
		t.Fatal("expected X-Metacore-Kernel-Version header to be populated")
	}
}

// TestServeAddonFrontend_DisabledWhenBasePathEmpty verifies the route
// 404s with a clear message when no FrontendBasePath is configured —
// hosts that never serve frontends from disk see a deterministic answer.
func TestServeAddonFrontend_DisabledWhenBasePathEmpty(t *testing.T) {
	db := setupTestDB(t)
	b, err := bridge.New(bridge.Config{DB: db})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	h, err := NewHandler(Deps{Bridge: b})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	app := fiber.New()
	app.Get("/addons/:key/frontend/*", h.ServeAddonFrontend)
	req := httptest.NewRequest("GET", "/addons/x/frontend/y.js", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestExecuteTool_NotRegistered verifies a 404 for unknown (addon, tool)
// pairs so the SDK client gets a deterministic envelope.
func TestExecuteTool_NotRegistered(t *testing.T) {
	db := setupTestDB(t)
	b, err := bridge.New(bridge.Config{DB: db})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	// Use a scoped registry so this test is hermetic — it doesn't share
	// state with whatever else has touched GlobalRegistry().
	reg := kerneltool.NewRegistry()
	h, err := NewHandler(Deps{Bridge: b, ToolRegistry: reg})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	app := fiber.New()
	orgID := uuid.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals("organization_id", orgID)
		return c.Next()
	})
	app.Post("/tools/execute", h.ExecuteTool)

	body := []byte(`{"addon_key":"x","tool_id":"y"}`)
	req := httptest.NewRequest("POST", "/tools/execute", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 404 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
}

// reference io to keep the import retained; some Go toolchains treat
// io.EOF references in inactive branches as ineligible for the imports
// pruner, which would otherwise drop the symbol.
var _ = io.EOF
