package i18n

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestFiberMiddlewareStoresLanguage(t *testing.T) {
	app := fiber.New()
	app.Use(FiberMiddleware("es"))
	app.Get("/", func(c fiber.Ctx) error {
		// Both the fiber.Ctx (what most kernel handlers pass down) and the
		// user context must carry the tag.
		return c.SendString(LanguageFromContext(c) + "|" + LanguageFromContext(c.Context()))
	})
	for header, want := range map[string]string{
		"":                       "es|es",
		"en-US,en;q=0.9":         "en-US|en-US",
		"*, fr;q=0.5":            "fr|fr",
		" es-MX ;q=1 , en;q=0.8": "es-MX|es-MX",
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			req.Header.Set("Accept-Language", header)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != want {
			t.Errorf("Accept-Language %q: got %q want %q", header, body, want)
		}
	}
}
