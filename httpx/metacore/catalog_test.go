package metacore

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/asteby/metacore-kernel/bridge"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// TestCatalog_RequiresOrg verifies the route returns 401 when no
// organization context is wired into the Fiber locals.
func TestCatalog_RequiresOrg(t *testing.T) {
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
	app.Get("/catalog", h.Catalog)
	req := httptest.NewRequest("GET", "/catalog", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestCatalog_EmptyWhenNoDir verifies the route returns an empty list
// instead of erroring when neither Deps.CatalogDir nor METACORE_CATALOG_DIR
// is set — hosts without a catalog directory still get a working route.
func TestCatalog_EmptyWhenNoDir(t *testing.T) {
	t.Setenv("METACORE_CATALOG_DIR", "")
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
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("organization_id", orgID)
		return c.Next()
	})
	app.Get("/catalog", h.Catalog)
	req := httptest.NewRequest("GET", "/catalog", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Items []interface{} `json:"items"`
		Total int           `json:"total"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 0 {
		t.Fatalf("expected empty items, got %v", out.Items)
	}
}
