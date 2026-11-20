// Package errcodes is the kernel's catalog of stable error codes: code ->
// message per language, with {param} placeholders. The kernel returns a
// stable `code` + `params` and an English fallback `message`; hosts and SDKs
// call Translate (or merge Messages() into their own bundle) to show the
// user's language without re-implementing the text. See docs/error-codes.md.
package errcodes

import (
	_ "embed"
	"encoding/json"
	"sync"

	"github.com/asteby/metacore-kernel/i18n"
)

//go:embed locales/errors.json
var catalogJSON []byte

var (
	once sync.Once
	cat  *i18n.Catalog
)

func load() *i18n.Catalog {
	once.Do(func() {
		var m map[string]map[string]string
		if err := json.Unmarshal(catalogJSON, &m); err != nil {
			m = nil // embedded asset; guarded by the package tests
		}
		cat = i18n.NewCatalog(m).WithFallback("en")
	})
	return cat
}

// Translate renders code in lang (BCP-47; "es-MX" falls back to "es", then to
// "en") interpolating params. ok is false when the code is not in the catalog.
func Translate(code, lang string, params map[string]any) (string, bool) {
	msg, ok := load().Lookup(lang, code)
	if !ok {
		return "", false
	}
	if len(params) == 0 {
		return msg, true
	}
	return i18n.Interpolate(msg, params), true
}

// Messages returns a copy of the whole catalog (lang -> code -> text) so a
// host can merge it into its translator.
func Messages() map[string]map[string]string { return load().Messages() }

// Translator adapts the catalog to i18n.Translator (language from ctx).
func Translator() i18n.Translator { return load() }

