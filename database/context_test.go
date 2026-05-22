package database_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/database"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

// runRequest mounts a single handler that calls WithFiberContext and copies
// the resulting session-level values into the provided capture map so the
// test can assert on them.
func runRequest(t *testing.T, locals map[string]any, headers map[string]string, ip string, capture map[string]any) {
	t.Helper()

	db := newTestDB(t)
	app := fiber.New()
	app.Get("/probe", func(c fiber.Ctx) error {
		for k, v := range locals {
			c.Locals(k, v)
		}
		session := database.WithFiberContext(c, db)
		for _, key := range []string{
			database.ContextKeyUserID,
			database.ContextKeyUserName,
			database.ContextKeyUserEmail,
			database.ContextKeyIPAddress,
			database.ContextKeyUserAgent,
		} {
			if v, ok := session.Get(key); ok {
				capture[key] = v
			}
		}
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/probe", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if ip != "" {
		req.RemoteAddr = ip + ":1234"
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

func TestWithFiberContext_PropagatesAllUserFields(t *testing.T) {
	uid := uuid.New()
	capture := map[string]any{}

	runRequest(t,
		map[string]any{
			"user_id":    uid,
			"user_name":  "Ada Lovelace",
			"user_email": "ada@example.com",
		},
		map[string]string{"User-Agent": "metacore-tests/1.0"},
		"203.0.113.7",
		capture,
	)

	gotUID, ok := capture[database.ContextKeyUserID].(*uuid.UUID)
	if !ok || gotUID == nil {
		t.Fatalf("user_id missing or wrong type: %#v", capture[database.ContextKeyUserID])
	}
	if *gotUID != uid {
		t.Fatalf("user_id mismatch: got %s want %s", *gotUID, uid)
	}

	if got, _ := capture[database.ContextKeyUserName].(string); got != "Ada Lovelace" {
		t.Fatalf("user_name mismatch: got %q", got)
	}
	if got, _ := capture[database.ContextKeyUserEmail].(string); got != "ada@example.com" {
		t.Fatalf("user_email mismatch: got %q", got)
	}
	if got, _ := capture[database.ContextKeyUserAgent].(string); got != "metacore-tests/1.0" {
		t.Fatalf("user_agent mismatch: got %q", got)
	}
	if got, _ := capture[database.ContextKeyIPAddress].(string); got == "" {
		t.Fatalf("ip_address should be populated, got empty")
	}
}

func TestWithFiberContext_NoUserLocals(t *testing.T) {
	capture := map[string]any{}

	runRequest(t, nil, map[string]string{"User-Agent": "ua/1"}, "", capture)

	// User fields must be absent when no locals were set …
	if _, ok := capture[database.ContextKeyUserID]; ok {
		t.Fatalf("user_id should not be set when locals are empty")
	}
	if _, ok := capture[database.ContextKeyUserName]; ok {
		t.Fatalf("user_name should not be set when locals are empty")
	}
	if _, ok := capture[database.ContextKeyUserEmail]; ok {
		t.Fatalf("user_email should not be set when locals are empty")
	}
	// … but ip + user_agent are always stamped.
	if _, ok := capture[database.ContextKeyIPAddress]; !ok {
		t.Fatalf("ip_address should always be set")
	}
	if got, _ := capture[database.ContextKeyUserAgent].(string); got != "ua/1" {
		t.Fatalf("user_agent mismatch: got %q", got)
	}
}

func TestWithFiberContext_IgnoresWrongTypeLocals(t *testing.T) {
	capture := map[string]any{}

	runRequest(t,
		map[string]any{
			// user_id as a plain string instead of uuid.UUID → must be ignored
			"user_id":    "not-a-uuid",
			"user_name":  42,                  // wrong type
			"user_email": []byte("bytes-not-string"), // wrong type
		},
		map[string]string{"User-Agent": "ua/2"},
		"",
		capture,
	)

	if _, ok := capture[database.ContextKeyUserID]; ok {
		t.Fatalf("user_id of wrong type should be skipped")
	}
	if _, ok := capture[database.ContextKeyUserName]; ok {
		t.Fatalf("user_name of wrong type should be skipped")
	}
	if _, ok := capture[database.ContextKeyUserEmail]; ok {
		t.Fatalf("user_email of wrong type should be skipped")
	}
}

func TestWithFiberContext_ReturnsFreshSession(t *testing.T) {
	// Sanity check: each invocation must return a NewDB session, not mutate
	// the caller's *gorm.DB. We compare pointer identity.
	db := newTestDB(t)
	app := fiber.New()

	var sessions []*gorm.DB
	app.Get("/probe", func(c fiber.Ctx) error {
		sessions = append(sessions, database.WithFiberContext(c, db))
		return c.SendString("ok")
	})

	for i := 0; i < 2; i++ {
		if _, err := app.Test(httptest.NewRequest("GET", "/probe", nil)); err != nil {
			t.Fatalf("Test: %v", err)
		}
	}

	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0] == sessions[1] {
		t.Fatalf("each call should yield a distinct session pointer")
	}
	if sessions[0] == db || sessions[1] == db {
		t.Fatalf("session must not be the caller's db pointer")
	}
}
