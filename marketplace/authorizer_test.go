package marketplace_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/marketplace"
	"github.com/gofiber/fiber/v3"
)

func TestInstall_AuthorizerDeniesNonAdmin(t *testing.T) {
	db := setupDB(t)
	app := fiber.New()
	allow := false
	h, err := marketplace.NewHandler(db, marketplace.WithAuthorizer(func(c fiber.Ctx) bool { return allow }))
	if err != nil {
		t.Fatal(err)
	}
	h.Mount(app, func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	})
	body := map[string]any{"addonKey": "demo", "version": "1.0.0"}
	resp, err := app.Test(makeInstallReq(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("sin rol: status %d, want 403", resp.StatusCode)
	}
	allow = true
	resp, err = app.Test(makeInstallReq(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("con rol: status %d, want 201", resp.StatusCode)
	}
}
