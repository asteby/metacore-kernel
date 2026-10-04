package log_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/asteby/metacore-kernel/dynamic"
	kernellog "github.com/asteby/metacore-kernel/log"
)

type fiberParentKey struct{}

func TestFiberMiddleware_StampsCorrelationAndKeepsParentContext(t *testing.T) {
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		parent := c.Context()
		if parent == nil {
			parent = context.Background()
		}
		c.SetContext(context.WithValue(parent, fiberParentKey{}, "kept"))
		return c.Next()
	})
	app.Use(kernellog.FiberMiddleware(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))))

	var gotCorr, gotParent string
	app.Get("/x", func(c fiber.Ctx) error {
		gotCorr = dynamic.CorrelationIDFromContext(c.Context())
		if v, _ := c.Context().Value(fiberParentKey{}).(string); v != "" {
			gotParent = v
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", "corr-fiber")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotCorr != "corr-fiber" {
		t.Fatalf("CorrelationIDFromContext = %q, want corr-fiber", gotCorr)
	}
	if gotParent != "kept" {
		t.Fatalf("parent context value = %q, want kept", gotParent)
	}
}
