package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestRegisterPublico_IgnoraRolDelBody(t *testing.T) {
	app, _ := setupTestApp(t)
	body, _ := json.Marshal(map[string]any{
		"name": "Mallory", "email": "m@example.com", "password": "hunter2!",
		"role": "admin", "extra": map[string]any{"role": "admin", "country": "MX"},
	})
	req := httptest.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("register: %v %v", err, resp)
	}
	var out struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	claims, err := ValidateToken(out.Data.Token, []byte("integration-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if claims.Role == "admin" {
		t.Fatalf("el registro público otorgó rol admin")
	}
}

func TestSanitizeRegisterExtra(t *testing.T) {
	out := sanitizeRegisterExtra(map[string]any{"Role": "admin", "is_admin": true, "country": "MX"})
	if len(out) != 1 || out["country"] != "MX" {
		t.Fatalf("extra mal filtrado: %v", out)
	}
}

func TestValidateToken_SecretoVacioFallaCerrado(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{UserID: uuid.New()})
	s, _ := tok.SignedString([]byte{})
	if _, err := ValidateToken(s, nil); err == nil {
		t.Fatal("debió fallar con secreto vacío")
	}
	if err := ValidateTokenWithClaims(s, nil, &Claims{}); err == nil {
		t.Fatal("debió fallar con secreto vacío")
	}
}

func TestValidateToken_RechazaHS384YPurpose(t *testing.T) {
	secret := []byte("k")
	c := Claims{UserID: uuid.New()}
	c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
	s384, _ := jwt.NewWithClaims(jwt.SigningMethodHS384, c).SignedString(secret)
	if _, err := ValidateToken(s384, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("HS384 debió rechazarse: %v", err)
	}
	c.Purpose = "2fa"
	s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
	if _, err := ValidateToken(s, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("purpose=2fa no debe valer como sesión: %v", err)
	}
	c.Purpose = PurposeSession
	s, _ = jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
	if _, err := ValidateToken(s, secret); err != nil {
		t.Fatalf("purpose=session debe valer: %v", err)
	}
}

func TestMiddleware_ValidateClaimsHook(t *testing.T) {
	secret := []byte("h")
	app := fiber.New()
	app.Use(Middleware(MiddlewareConfig{Secret: secret,
		ValidateClaims: func(c fiber.Ctx, cl *Claims) error { return errors.New("revocado") }}))
	app.Get("/p", func(c fiber.Ctx) error { return c.SendString("ok") })
	tok, _, _ := GenerateToken(Claims{UserID: uuid.New()}, secret, time.Hour)
	req := httptest.NewRequest("GET", "/p", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := app.Test(req)
	if resp.StatusCode != 401 {
		t.Fatalf("esperaba 401, got %d", resp.StatusCode)
	}
}
