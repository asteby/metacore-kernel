package dynamic

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// TestOptionsEnvelopeShape locks in the v0.9.0 envelope contract for
// `GET /api/options/:model`. The route now mirrors every other dynamic
// handler — `{success, data, meta}` — with the static/dynamic discriminator
// nested under `meta.type` instead of riding alongside `data`.
func TestOptionsEnvelopeShape(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"status": {
				Type: "static",
				Options: []StaticOption{
					{Value: "active", Label: "Active"},
					{Value: "inactive", Label: "Inactive"},
				},
			},
		},
	}), nil)

	app := fiber.New()
	h := NewHandler(svc, nil) // options handler does not require auth
	h.MountOptions(app)

	req := httptest.NewRequest("GET", "/options/test_products?field=status", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}

	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if ok, _ := env["success"].(bool); !ok {
		t.Fatalf("success != true: %v", env)
	}
	data, ok := env["data"].([]any)
	if !ok {
		t.Fatalf("data must be an array, got %T", env["data"])
	}
	if len(data) != 2 {
		t.Fatalf("expected 2 options, got %d", len(data))
	}
	meta, ok := env["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta envelope missing, got %T", env["meta"])
	}
	if got, _ := meta["type"].(string); got != "static" {
		t.Fatalf("meta.type = %q, want static", got)
	}
	if got, _ := meta["count"].(float64); int(got) != 2 {
		t.Fatalf("meta.count = %v, want 2", got)
	}
	// `type` MUST NOT remain at the envelope root in v0.9.0 — that is the
	// breaking change consumers need to react to.
	if _, present := env["type"]; present {
		t.Fatalf("v0.9.0 envelope must not expose root-level `type`, got %v", env)
	}
}
