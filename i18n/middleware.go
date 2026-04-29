package i18n

import (
	"strings"

	"github.com/gofiber/fiber/v3"
)

// FiberMiddleware reads the `Accept-Language` header on the incoming
// request, picks the highest-priority tag, and stores it in the user
// context via WithLanguage. Downstream handlers and Service transformers
// can pull the tag with LanguageFromContext.
//
// `defaults` is the language used when the header is missing or empty.
// Pass "es" or "en" depending on the app's primary language.
//
// Quality factors (`;q=0.8`) are honoured to the extent that the first
// non-zero tag wins — the kernel does not reorder by q because real
// clients almost never disagree with the natural list order.
func FiberMiddleware(defaultLang string) fiber.Handler {
	return func(c fiber.Ctx) error {
		lang := pickLanguage(c.Get("Accept-Language"), defaultLang)
		ctx := WithLanguage(c, lang)
		c.SetContext(ctx)
		return c.Next()
	}
}

// pickLanguage extracts the first language tag from an Accept-Language
// header value, ignoring quality factors. Falls back to `def` when the
// header is empty or has no usable tag.
func pickLanguage(header, def string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return def
	}
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		// Strip ;q=... parameters.
		if i := strings.Index(tag, ";"); i >= 0 {
			tag = strings.TrimSpace(tag[:i])
		}
		if tag != "" && tag != "*" {
			return tag
		}
	}
	return def
}
