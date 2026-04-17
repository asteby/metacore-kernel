package auth

// TODO: requires modelbase — this integration test exercises Login+Register
// end-to-end via a Fiber app backed by an in-memory SQLite DB. It instantiates
// a concrete User type that embeds modelbase.BaseUser and satisfies
// modelbase.AuthUser. Enable it once modelbase lands in the kernel.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelbase" // TODO: requires modelbase
	"github.com/gofiber/fiber/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// testUser is a minimal concrete User model for the integration test. Once
// modelbase exists this should embed modelbase.BaseUser. For now the embed
// is declared directly so the compiler tells us exactly what's missing.
type testUser struct {
	modelbase.BaseUser
}

func setupTestApp(t *testing.T) (*fiber.App, *Service) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite doesn't understand gen_random_uuid() (the default on BaseUUIDModel
	// for Postgres), so create the schema manually. BeforeCreate fills the UUID.
	if err := db.Exec(`CREATE TABLE users (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		name TEXT,
		email TEXT UNIQUE,
		password_hash TEXT,
		role TEXT DEFAULT 'agent',
		avatar TEXT,
		last_login_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}

	svc := New(db, Config{
		JWTSecret:  []byte("integration-test-secret"),
		JWTIssuer:  "test",
		JWTExpiry:  time.Hour,
		BcryptCost: 4,
	}).WithUserModel(func() modelbase.AuthUser { return &testUser{} })

	h := NewHandler(svc)
	mw := Middleware(MiddlewareConfig{Secret: svc.Config().JWTSecret})

	app := fiber.New()
	h.Mount(app.Group("/auth"), mw)

	return app, svc
}

func TestHandler_RegisterThenLoginThenMe(t *testing.T) {
	app, _ := setupTestApp(t)

	// Register
	regBody, _ := json.Marshal(map[string]string{
		"name":     "Alice",
		"email":    "alice@example.com",
		"password": "hunter2!",
	})
	req := httptest.NewRequest("POST", "/auth/register", bytes.NewReader(regBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("register test: %v", err)
	}
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("register: expected 201, got %d body=%s", resp.StatusCode, string(b))
	}

	var registered struct {
		Success bool `json:"success"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode register: %v body=%s", err, string(body))
	}
	if !registered.Success || registered.Data.Token == "" {
		t.Fatalf("bad register payload: %s", string(body))
	}

	// Login
	loginBody, _ := json.Marshal(map[string]string{
		"email":    "alice@example.com",
		"password": "hunter2!",
	})
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("login test: %v", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: expected 200, got %d body=%s", resp.StatusCode, string(b))
	}

	var loggedIn struct {
		Success bool `json:"success"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	body, _ = io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &loggedIn); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if !loggedIn.Success || loggedIn.Data.Token == "" {
		t.Fatalf("bad login payload: %s", string(body))
	}

	// Me
	req = httptest.NewRequest("GET", "/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+loggedIn.Data.Token)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("me test: %v", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("me: expected 200, got %d body=%s", resp.StatusCode, string(b))
	}
}

func TestHandler_LoginWrongPassword(t *testing.T) {
	app, svc := setupTestApp(t)

	// Seed user via Register path.
	_, err := svc.Register(nil, RegisterInput{
		Name:     "Bob",
		Email:    "bob@example.com",
		Password: "correct",
	})
	if err != nil {
		t.Fatalf("seed register: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"email":    "bob@example.com",
		"password": "wrong",
	})
	req := httptest.NewRequest("POST", "/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}
