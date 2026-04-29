package permission

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

func newGateApp(t *testing.T, svc *Service, lookup UserLookup, caps ...Capability) *fiber.App {
	t.Helper()
	app := fiber.New()
	app.Get("/protected",
		svc.Gate(lookup, caps...),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)
	return app
}

func TestGate_Allow(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "admin"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Cap("users", "delete")}}, nil)

	app := newGateApp(t, svc,
		func(c fiber.Ctx) modelbase.AuthUser { return user },
		Cap("users", "delete"),
	)

	resp, err := app.Test(httptest.NewRequest("GET", "/protected", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d body=%s", resp.StatusCode, string(b))
	}
}

func TestGate_Deny(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "agent"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Cap("users", "delete")}}, nil)

	app := newGateApp(t, svc,
		func(c fiber.Ctx) modelbase.AuthUser { return user },
		Cap("users", "delete"),
	)
	resp, err := app.Test(httptest.NewRequest("GET", "/protected", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

func TestGate_Unauthenticated(t *testing.T) {
	svc := newSvc(t, nil, nil)
	app := newGateApp(t, svc,
		func(c fiber.Ctx) modelbase.AuthUser { return nil },
		Cap("users", "delete"),
	)
	resp, err := app.Test(httptest.NewRequest("GET", "/protected", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestGate_SuperRoleBypass(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "owner"}
	svc := newSvc(t, nil, nil)
	app := newGateApp(t, svc,
		func(c fiber.Ctx) modelbase.AuthUser { return user },
		Cap("users", "delete"),
	)
	resp, err := app.Test(httptest.NewRequest("GET", "/protected", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 super role bypass, got %d", resp.StatusCode)
	}
}

func TestGate_NoCapsActsAsAuthOnly(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "agent"}
	svc := newSvc(t, nil, nil)

	app := fiber.New()
	app.Get("/protected",
		svc.Gate(func(c fiber.Ctx) modelbase.AuthUser { return user }),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)

	resp, err := app.Test(httptest.NewRequest("GET", "/protected", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

func TestGate_ModeAny(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "admin"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Cap("a", "b")}}, nil)

	app := fiber.New()
	app.Get("/any",
		svc.GateWith(
			func(c fiber.Ctx) modelbase.AuthUser { return user },
			GateConfig{Mode: ModeAny},
			Cap("x", "y"), Cap("a", "b"),
		),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)

	resp, err := app.Test(httptest.NewRequest("GET", "/any", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 ModeAny hit, got %d", resp.StatusCode)
	}
}

func TestGate_CustomResponders(t *testing.T) {
	svc := newSvc(t, nil, nil)
	deniedCalled := false

	app := fiber.New()
	app.Get("/p",
		svc.GateWith(
			func(c fiber.Ctx) modelbase.AuthUser {
				return &fakeUser{id: uuid.New(), role: "agent"}
			},
			GateConfig{OnDenied: func(c fiber.Ctx, err error) error {
				deniedCalled = true
				return c.Status(418).SendString("teapot")
			}},
			Cap("x", "y"),
		),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)

	resp, err := app.Test(httptest.NewRequest("GET", "/p", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 418 || !deniedCalled {
		t.Fatalf("custom responder not used: status=%d called=%v", resp.StatusCode, deniedCalled)
	}
}

func TestGate_TopLevelFactory(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "admin"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Cap("a", "b")}}, nil)

	app := fiber.New()
	app.Get("/p",
		Gate(svc, func(c fiber.Ctx) modelbase.AuthUser { return user }, Cap("a", "b")),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)
	resp, err := app.Test(httptest.NewRequest("GET", "/p", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}
