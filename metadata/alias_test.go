package metadata

import (
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelalias"
	"github.com/gofiber/fiber/v3"
)

func TestHandler_ResolvesModelAliases(t *testing.T) {
	key := registerHandlerModel(t, "Aliased")
	idx := modelalias.New()
	if err := idx.Add(key, "aliased_tbl", []string{"order-items", "client"}); err != nil {
		t.Fatal(err)
	}
	svc := New(Config{CacheTTL: time.Minute, ModelAliases: idx})
	app := fiber.New()
	NewHandler(svc).Mount(app.Group("/metadata"))

	for _, slug := range []string{key, "order-items", "Order_Items", "order-item", "client", "clients"} {
		status, env := doRequest(t, app, "GET", "/metadata/table/"+slug)
		if status != fiber.StatusOK {
			t.Errorf("table/%s: %d (%s)", slug, status, env.Message)
		}
		if status, _ = doRequest(t, app, "GET", "/metadata/modal/"+slug); status != fiber.StatusOK {
			t.Errorf("modal/%s: %d", slug, status)
		}
	}
	if status, _ := doRequest(t, app, "GET", "/metadata/table/unknown-thing"); status != fiber.StatusNotFound {
		t.Errorf("unknown slug must stay 404, got %d", status)
	}
}

func TestHandler_WithoutAliasesIsUnchanged(t *testing.T) {
	app, _ := newTestHandler(t)
	registerHandlerModel(t, "Plain")
	if status, _ := doRequest(t, app, "GET", "/metadata/table/order-items"); status != fiber.StatusNotFound {
		t.Fatalf("no aliases configured: want 404, got %d", status)
	}
}

func TestCanonicalModelPrefersRealModel(t *testing.T) {
	key := registerHandlerModel(t, "Real")
	idx := modelalias.New()
	_ = idx.Add("Other", "others", []string{key}) // alias equal to a real model's key
	svc := New(Config{ModelAliases: idx})
	if got := svc.CanonicalModel(key); got != key {
		t.Fatalf("a registered model must win over an alias, got %q", got)
	}
}
