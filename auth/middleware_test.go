package auth

import (
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func newTestApp(secret []byte) *fiber.App {
	app := fiber.New()
	app.Use(Middleware(MiddlewareConfig{Secret: secret}))
	app.Get("/protected", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"user_id":         GetUserID(c).String(),
			"organization_id": GetOrganizationID(c).String(),
			"role":            GetRole(c),
		})
	})
	return app
}

func TestMiddleware_NoToken(t *testing.T) {
	app := newTestApp([]byte("s"))
	req := httptest.NewRequest("GET", "/protected", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestMiddleware_InvalidToken(t *testing.T) {
	app := newTestApp([]byte("s"))
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer not-a-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestMiddleware_ValidToken(t *testing.T) {
	secret := []byte("super-secret")
	app := newTestApp(secret)

	userID := uuid.New()
	orgID := uuid.New()
	token, _, err := GenerateToken(Claims{
		UserID:         userID,
		OrganizationID: orgID,
		Email:          "x@y.z",
		Role:           "admin",
	}, secret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(b))
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got == "" {
		t.Fatal("empty body")
	}
}

func TestMiddleware_QueryParamToken(t *testing.T) {
	secret := []byte("s2")
	app := newTestApp(secret)

	token, _, err := GenerateToken(Claims{UserID: uuid.New()}, secret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	req := httptest.NewRequest("GET", "/protected?token="+token, nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestMiddleware_Skipper(t *testing.T) {
	app := fiber.New()
	app.Use(Middleware(MiddlewareConfig{
		Secret:  []byte("s"),
		Skipper: func(c *fiber.Ctx) bool { return c.Path() == "/public" },
	}))
	app.Get("/public", func(c *fiber.Ctx) error { return c.SendString("ok") })

	req := httptest.NewRequest("GET", "/public", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 via skipper, got %d", resp.StatusCode)
	}
}

func TestMiddleware_ExpiredToken(t *testing.T) {
	secret := []byte("s3")
	app := newTestApp(secret)

	past := time.Now().Add(-time.Hour)
	claims := Claims{UserID: uuid.New()}
	claims.IssuedAt = jwt.NewNumericDate(past.Add(-time.Minute))
	claims.ExpiresAt = jwt.NewNumericDate(past)
	token, _, err := GenerateToken(claims, secret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 for expired, got %d", resp.StatusCode)
	}
}
