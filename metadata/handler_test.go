package metadata

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/gofiber/fiber/v3"
)

// registerHandlerModel is a handler-test-scoped variant of registerFakeModel
// that uses a distinct prefix so the two test files do not contend on keys.
func registerHandlerModel(t *testing.T, title string) string {
	t.Helper()
	key := fmt.Sprintf("metadata_handler_test_%s_%d", t.Name(), time.Now().UnixNano())
	titleCopy := title
	modelbase.Register(key, func() modelbase.ModelDefiner {
		return &fakeModel{key: key, title: titleCopy}
	})
	return key
}

func newTestHandler(t *testing.T) (*fiber.App, *Service) {
	t.Helper()
	svc := New(Config{CacheTTL: time.Minute})
	h := NewHandler(svc)

	app := fiber.New()
	h.Mount(app.Group("/metadata"))
	return app, svc
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func doRequest(t *testing.T, app *fiber.App, method, path string) (int, envelope) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (body=%s)", err, string(body))
	}
	return resp.StatusCode, env
}

func TestHandler_GetTable_Found(t *testing.T) {
	app, _ := newTestHandler(t)
	key := registerHandlerModel(t, "H-Users")

	status, env := doRequest(t, app, "GET", "/metadata/table/"+key)
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d (msg=%s)", status, env.Message)
	}
	if !env.Success {
		t.Fatalf("expected success=true, got false")
	}

	var meta modelbase.TableMetadata
	if err := json.Unmarshal(env.Data, &meta); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if meta.Title != "H-Users" {
		t.Fatalf("expected title H-Users, got %q", meta.Title)
	}
}

func TestHandler_GetTable_NotFound(t *testing.T) {
	app, _ := newTestHandler(t)

	status, env := doRequest(t, app, "GET", "/metadata/table/this_model_does_not_exist")
	if status != fiber.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
	if env.Success {
		t.Fatalf("expected success=false, got true")
	}
	if env.Message == "" {
		t.Fatalf("expected non-empty message")
	}
}

func TestHandler_GetModal_Found(t *testing.T) {
	app, _ := newTestHandler(t)
	key := registerHandlerModel(t, "H-Products")

	status, env := doRequest(t, app, "GET", "/metadata/modal/"+key)
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d (msg=%s)", status, env.Message)
	}

	var meta modelbase.ModalMetadata
	if err := json.Unmarshal(env.Data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.Title != "H-Products" {
		t.Fatalf("expected title H-Products, got %q", meta.Title)
	}
}

func TestHandler_GetModal_NotFound(t *testing.T) {
	app, _ := newTestHandler(t)

	status, _ := doRequest(t, app, "GET", "/metadata/modal/absent_model")
	if status != fiber.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

func TestHandler_GetAll_ReturnsEveryModel(t *testing.T) {
	app, _ := newTestHandler(t)
	k1 := registerHandlerModel(t, "All1")
	k2 := registerHandlerModel(t, "All2")

	status, env := doRequest(t, app, "GET", "/metadata/all")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	var all AllMetadata
	if err := json.Unmarshal(env.Data, &all); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := all.Tables[k1]; !ok {
		t.Fatalf("Tables missing %q", k1)
	}
	if _, ok := all.Tables[k2]; !ok {
		t.Fatalf("Tables missing %q", k2)
	}
	if _, ok := all.Modals[k1]; !ok {
		t.Fatalf("Modals missing %q", k1)
	}
	if all.Version == "" {
		t.Fatalf("Version must be set")
	}
}

func TestHandler_Mount_AppliesMiddleware(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	h := NewHandler(svc)

	var mwRan bool
	mw := func(c fiber.Ctx) error {
		mwRan = true
		return c.Next()
	}

	app := fiber.New()
	h.Mount(app.Group("/metadata"), mw)

	key := registerHandlerModel(t, "MW")
	_, _ = doRequest(t, app, "GET", "/metadata/table/"+key)
	if !mwRan {
		t.Fatalf("middleware was not invoked")
	}
}

func TestHandler_Mount_MiddlewareCanBlock(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	h := NewHandler(svc)

	blockMW := func(c fiber.Ctx) error {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "blocked",
		})
	}

	app := fiber.New()
	h.Mount(app.Group("/metadata"), blockMW)

	registerHandlerModel(t, "Blocked")
	status, env := doRequest(t, app, "GET", "/metadata/all")
	if status != fiber.StatusUnauthorized {
		t.Fatalf("expected 401 from middleware, got %d", status)
	}
	if env.Message != "blocked" {
		t.Fatalf("expected blocked message, got %q", env.Message)
	}
}
