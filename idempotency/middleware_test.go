package idempotency_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/idempotency"
	"github.com/gofiber/fiber/v2"
)

func newTestApp(handlerCalls *int32) *fiber.App {
	app := fiber.New()
	store := idempotency.NewInMemoryStore(0)
	app.Use(idempotency.Middleware(idempotency.Config{Store: store, TTL: time.Minute}))
	app.Post("/create", func(c *fiber.Ctx) error {
		atomic.AddInt32(handlerCalls, 1)
		return c.Status(201).JSON(fiber.Map{"id": atomic.LoadInt32(handlerCalls)})
	})
	return app
}

func do(t *testing.T, app *fiber.App, key string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/create", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(idempotency.HeaderKey, key)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	return resp.StatusCode, string(body[:n]), resp.Header.Get("Idempotent-Replay")
}

func TestMiddleware_NoKey_PassesThrough(t *testing.T) {
	var calls int32
	app := newTestApp(&calls)
	for i := 0; i < 3; i++ {
		status, _, replay := do(t, app, "")
		if status != 201 {
			t.Fatalf("got %d", status)
		}
		if replay != "" {
			t.Fatalf("replay header should be empty without key")
		}
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("expected 3 handler calls, got %d", calls)
	}
}

func TestMiddleware_SameKey_ReplaysFirstResponse(t *testing.T) {
	var calls int32
	app := newTestApp(&calls)

	status, body1, replay1 := do(t, app, "abc")
	if status != 201 || replay1 != "" {
		t.Fatalf("first call: status=%d replay=%q body=%s", status, replay1, body1)
	}

	status, body2, replay2 := do(t, app, "abc")
	if status != 201 || replay2 != "true" || body2 != body1 {
		t.Fatalf("replay mismatch: status=%d replay=%q body1=%s body2=%s", status, replay2, body1, body2)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler should be called once, got %d", got)
	}
}

func TestMiddleware_DifferentKeys_ProduceDistinctResponses(t *testing.T) {
	var calls int32
	app := newTestApp(&calls)
	_, body1, _ := do(t, app, "key-a")
	_, body2, _ := do(t, app, "key-b")
	if body1 == body2 {
		t.Fatalf("different keys should produce different responses; both are %s", body1)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 handler calls, got %d", calls)
	}
}

func TestInMemoryStore_TTL_EvictsExpired(t *testing.T) {
	s := idempotency.NewInMemoryStore(0)
	s.Put("k", idempotency.Stored{StatusCode: 200, ExpiresAt: time.Now().Add(-time.Minute)})
	if _, ok := s.Get("k"); ok {
		t.Fatal("expired entry should not be returned")
	}
}

func TestInMemoryStore_Eviction(t *testing.T) {
	s := idempotency.NewInMemoryStore(2)
	in := func(k string) {
		s.Put(k, idempotency.Stored{StatusCode: 200, ExpiresAt: time.Now().Add(time.Hour)})
	}
	in("a")
	in("b")
	in("c") // should evict "a"
	if _, ok := s.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("c should still be present")
	}
}
