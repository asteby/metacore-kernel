package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// stubDispatcher is a per-test ActionDispatcher whose Dispatch is driven by a
// captured closure. Tests rebind .fn before each scenario.
type stubDispatcher struct {
	calls int
	last  ActionRequest
	fn    func(ctx context.Context, req ActionRequest) (ActionResponse, error)
}

func (s *stubDispatcher) Dispatch(ctx context.Context, req ActionRequest) (ActionResponse, error) {
	s.calls++
	s.last = req
	if s.fn == nil {
		return ActionResponse{Success: true}, nil
	}
	return s.fn(ctx, req)
}

// fakeUserResolver threads a fixed user into every request — auth middleware
// is not what we are exercising here.
func fakeUserResolver(u modelbase.AuthUser) UserResolver {
	return func(_ fiber.Ctx) modelbase.AuthUser { return u }
}

// actionFixture holds the wiring shared across handler tests: a clean DB, a
// service with a stub dispatcher per Trigger.Type, and a manifest action
// resolver populated from a map. setupActionFixture(...) returns the fiber
// app already mounted on /dynamic.
type actionFixture struct {
	db        *gorm.DB
	svc       *Service
	app       *fiber.App
	wasm      *stubDispatcher
	webhook   *stubDispatcher
	user      *fakeUser
	productID uuid.UUID
	actions   map[string]map[string]*manifest.ActionDef // model → key → def
}

func setupActionFixture(t *testing.T) *actionFixture {
	t.Helper()
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })

	wasm := &stubDispatcher{}
	webhook := &stubDispatcher{}

	fx := &actionFixture{
		db:      db,
		wasm:    wasm,
		webhook: webhook,
		actions: map[string]map[string]*manifest.ActionDef{},
	}

	meta := metadata.New(metadata.Config{CacheTTL: -1})
	fx.svc = New(Config{
		DB:       db,
		Metadata: meta,
		ActionResolver: func(_ context.Context, model, key string) (*manifest.ActionDef, bool) {
			if m, ok := fx.actions[model]; ok {
				if def, ok := m[key]; ok {
					return def, true
				}
			}
			return nil, false
		},
		ActionDispatchers: map[string]ActionDispatcher{
			"wasm":    wasm,
			"webhook": webhook,
			// "noop" intentionally omitted — the kernel registers
			// NoopDispatcher by default and the test asserts the
			// built-in.
		},
	})

	fx.user = newUser(uuid.New())
	created := createProduct(t, fx.svc, fx.user, "Widget", 9.99)
	id, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}
	fx.productID = id

	h := NewHandler(fx.svc, fakeUserResolver(fx.user))
	app := fiber.New()
	h.Mount(app)
	fx.app = app

	return fx
}

// registerAction wires an ActionDef into the fixture's resolver.
func (fx *actionFixture) registerAction(model string, def *manifest.ActionDef) {
	if fx.actions[model] == nil {
		fx.actions[model] = map[string]*manifest.ActionDef{}
	}
	fx.actions[model][def.Key] = def
}

// invokeAction issues POST /dynamic/:model/:id/action/:key with the given JSON
// body and returns (status, parsed envelope).
func invokeAction(t *testing.T, app *fiber.App, model string, id uuid.UUID, key, body string) (int, map[string]any) {
	t.Helper()
	url := "/dynamic/" + model + "/" + id.String() + "/action/" + key
	req := httptest.NewRequest("POST", url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode envelope: %v (body=%s)", err, string(raw))
		}
	}
	return resp.StatusCode, env
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestAction_Noop_ReturnsEnvelope(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:     "ping",
		Trigger: &manifest.ActionTrigger{Type: "noop"},
	})

	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "ping", `{}`)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; env=%v", status, env)
	}
	if env["success"] != true {
		t.Fatalf("success = %v, want true", env["success"])
	}
	meta, ok := env["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing/invalid: %v", env["meta"])
	}
	if meta["model"] != "test_products" || meta["action"] != "ping" || meta["trigger_type"] != "noop" {
		t.Fatalf("kernel meta missing/wrong: %v", meta)
	}
	if meta["no_op"] != true {
		t.Fatalf("expected meta.no_op=true (built-in noop dispatcher), got %v", meta["no_op"])
	}
}

func TestAction_Wasm_Success_Commits(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key: "stamp",
		Trigger: &manifest.ActionTrigger{
			Type:    "wasm",
			Export:  "stamp",
			RunInTx: true,
		},
	})

	fx.wasm.fn = func(_ context.Context, req ActionRequest) (ActionResponse, error) {
		if req.DB == nil {
			t.Fatalf("expected DB transaction handle, got nil")
		}
		// Mutate inside the tx to prove the kernel commits on success.
		if err := req.DB.Exec(`UPDATE test_products SET name = ? WHERE id = ?`, "Stamped", req.Row["id"]).Error; err != nil {
			t.Fatalf("tx exec: %v", err)
		}
		return ActionResponse{
			Success: true,
			Data:    map[string]any{"queue": "tier-2"},
			Meta:    map[string]any{"trigger_type": "guest-supplied-should-be-overridden"},
		}, nil
	}

	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "stamp", `{"reason":"x"}`)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; env=%v", status, env)
	}
	if env["success"] != true {
		t.Fatalf("success = %v, want true", env["success"])
	}
	data, _ := env["data"].(map[string]any)
	if data["queue"] != "tier-2" {
		t.Fatalf("data forwarded wrong: %v", env["data"])
	}
	meta, _ := env["meta"].(map[string]any)
	if meta["trigger_type"] != "wasm" {
		t.Fatalf("kernel meta.trigger_type should win over guest, got %v", meta["trigger_type"])
	}
	if meta["rolled_back"] != false {
		t.Fatalf("meta.rolled_back = %v, want false", meta["rolled_back"])
	}

	// Verify the tx committed: the row name is now "Stamped".
	if got := readProductName(t, fx.db, fx.productID); got != "Stamped" {
		t.Fatalf("tx did not commit: name = %q", got)
	}
}

func TestAction_Wasm_FailureRollsBack(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key: "decline",
		Trigger: &manifest.ActionTrigger{
			Type:    "wasm",
			Export:  "decline",
			RunInTx: true,
		},
	})

	fx.wasm.fn = func(_ context.Context, req ActionRequest) (ActionResponse, error) {
		// Mutate then decline — the kernel must rollback so the mutation
		// is invisible after Dispatch returns.
		if err := req.DB.Exec(`UPDATE test_products SET name = ? WHERE id = ?`, "ShouldRollBack", req.Row["id"]).Error; err != nil {
			t.Fatalf("tx exec: %v", err)
		}
		return ActionResponse{
			Success: false,
			Error:   &ActionError{Code: "declined", Message: "business rule violated"},
		}, nil
	}

	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "decline", `{}`)
	if status != fiber.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; env=%v", status, env)
	}
	if env["success"] != false {
		t.Fatalf("success = %v, want false", env["success"])
	}
	errBlock, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("error block missing: %v", env)
	}
	if errBlock["code"] != "declined" || errBlock["message"] != "business rule violated" {
		t.Fatalf("error forwarded wrong: %v", errBlock)
	}
	meta, _ := env["meta"].(map[string]any)
	if meta["rolled_back"] != true {
		t.Fatalf("meta.rolled_back = %v, want true", meta["rolled_back"])
	}

	// Verify the row name reverted (rollback worked).
	if got := readProductName(t, fx.db, fx.productID); got != "Widget" {
		t.Fatalf("tx not rolled back: name = %q", got)
	}
}

// readProductName executes a Raw SQL select bypassing GORM's auto-model so the
// rollback assertion does not depend on registering a model handle.
func readProductName(t *testing.T, db *gorm.DB, id uuid.UUID) string {
	t.Helper()
	var name string
	if err := db.Raw(`SELECT name FROM test_products WHERE id = ?`, id.String()).Scan(&name).Error; err != nil {
		t.Fatalf("readProductName: %v", err)
	}
	return name
}

func TestAction_Webhook_NoTx(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:     "notify",
		Trigger: &manifest.ActionTrigger{Type: "webhook"},
	})

	fx.webhook.fn = func(_ context.Context, req ActionRequest) (ActionResponse, error) {
		if req.DB != nil {
			t.Fatalf("webhook trigger should not receive a tx handle")
		}
		return ActionResponse{Success: true, Data: map[string]any{"sent": true}}, nil
	}

	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "notify", ``)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; env=%v", status, env)
	}
	meta, _ := env["meta"].(map[string]any)
	if _, present := meta["rolled_back"]; present {
		t.Fatalf("rolled_back should not be set for non-tx triggers; meta=%v", meta)
	}
	if meta["trigger_type"] != "webhook" {
		t.Fatalf("trigger_type = %v, want webhook", meta["trigger_type"])
	}
}

func TestAction_DispatcherErrorBubbles(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key: "boom",
		Trigger: &manifest.ActionTrigger{
			Type:    "wasm",
			Export:  "boom",
			RunInTx: true,
		},
	})

	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		return ActionResponse{}, errors.New("guest crashed")
	}

	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "boom", `{}`)
	if status != fiber.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; env=%v", status, env)
	}
	if env["success"] != false {
		t.Fatalf("success = %v, want false", env["success"])
	}
}

func TestAction_404_ActionNotFound(t *testing.T) {
	fx := setupActionFixture(t)
	// no actions registered

	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "ghost", `{}`)
	if status != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404; env=%v", status, env)
	}
	if env["success"] != false {
		t.Fatalf("success = %v, want false", env["success"])
	}
}

func TestAction_404_RecordNotFound(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:     "ping",
		Trigger: &manifest.ActionTrigger{Type: "noop"},
	})

	status, _ := invokeAction(t, fx.app, "test_products", uuid.New(), "ping", `{}`)
	if status != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestAction_400_InvalidID(t *testing.T) {
	fx := setupActionFixture(t)
	req := httptest.NewRequest("POST", "/dynamic/test_products/not-a-uuid/action/ping", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := fx.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAction_501_NoActionResolver(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	svc := New(Config{DB: db, Metadata: meta}) // no ActionResolver wired
	user := newUser(uuid.New())
	created := createProduct(t, svc, user, "X", 1)
	id, _ := uuid.Parse(created["id"].(string))

	app := fiber.New()
	NewHandler(svc, fakeUserResolver(user)).Mount(app)

	status, _ := invokeAction(t, app, "test_products", id, "anything", `{}`)
	if status != fiber.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", status)
	}
}

func TestAction_501_UnsupportedTriggerType(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:     "exotic",
		Trigger: &manifest.ActionTrigger{Type: "event"}, // not registered
	})

	status, _ := invokeAction(t, fx.app, "test_products", fx.productID, "exotic", `{}`)
	if status != fiber.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", status)
	}
}
