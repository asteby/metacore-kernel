// Package i18n is the kernel's translator contract. Apps that want
// localized metadata (and any other server-rendered strings) implement
// the Translator interface and inject it via host.AppConfig.
//
// The kernel intentionally ships no concrete implementation: bundles
// vary across apps (some use go-i18n with JSON files, some hit a CMS,
// some embed YAML). What the kernel provides is the wire-up — request
// language extraction, ctx propagation, and a ready-made metadata
// translator that walks the wire payload.
//
// Typical wiring inside a starter:
//
//	tr := mystarter.NewTranslator()              // app-owned implementation
//	app := host.NewApp(host.AppConfig{
//	    DB:         db,
//	    JWTSecret:  secret,
//	    Translator: tr,                          // kernel registers
//	})                                            // metadata transformers automatically
//
// Once wired, every label / placeholder / option label in TableMetadata
// and ModalMetadata gets a chance to be translated based on the
// `Accept-Language` header, falling back to the configured default.
package i18n

import "context"

// Translator turns an i18n key (e.g. "models.customers.table.title") into
// the localized string for the language stored in ctx via WithLanguage.
//
// Implementations should:
//   - Return the original key when no translation is found, so callers can
//     spot missing keys without crashing.
//   - Be safe to call concurrently — a Service may run transformer chains
//     across goroutines.
type Translator interface {
	Translate(ctx context.Context, key string, args ...any) string
}

// TranslatorFunc adapts an ordinary function into a Translator.
type TranslatorFunc func(ctx context.Context, key string, args ...any) string

// Translate satisfies Translator.
func (f TranslatorFunc) Translate(ctx context.Context, key string, args ...any) string {
	return f(ctx, key, args...)
}

// languageKey is unexported so apps can't collide on the ctx key. Use
// WithLanguage / LanguageFromContext exclusively.
type languageKey struct{}

// WithLanguage returns a child context tagged with the given BCP-47
// language tag (e.g. "es", "en-US"). An empty lang is preserved so
// callers can detect "no preference" downstream.
func WithLanguage(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, languageKey{}, lang)
}

// LanguageFromContext extracts the language tag stored by WithLanguage.
// Returns an empty string when the context was never tagged.
func LanguageFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(languageKey{}).(string)
	return v
}
