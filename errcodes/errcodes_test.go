package errcodes

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/importer"
)

func TestCatalogParityAndTranslate(t *testing.T) {
	m := Messages()
	if len(m["es"]) == 0 || len(m["es"]) != len(m["en"]) {
		t.Fatalf("es/en out of sync: %d vs %d", len(m["es"]), len(m["en"]))
	}
	for k := range m["en"] {
		if _, ok := m["es"][k]; !ok {
			t.Errorf("code %q missing in es", k)
		}
	}
	for _, c := range append(importer.IssueCodes(), "source_not_found", "source_not_eligible", "source_line_not_found", "exceeds_remaining", "unknown_filter") {
		if _, ok := m["en"][c]; !ok {
			t.Errorf("emitted code %q not in the catalog", c)
		}
	}
	got, ok := Translate("exceeds_remaining", "es-MX", map[string]any{"line": 2, "requested": 3, "remaining": 1, "source": "V"})
	if !ok || !strings.HasPrefix(got, "Renglón 2: la cantidad 3") || strings.Contains(got, "{") {
		t.Fatalf("es translate = %q %v", got, ok)
	}
	if en, _ := Translate("importer.email", "fr", map[string]any{"column": "E", "value": "x"}); !strings.Contains(en, "valid email") {
		t.Fatalf("fallback to en failed: %q", en)
	}
	if _, ok := Translate("nope", "en", nil); ok {
		t.Fatal("unknown code must report !ok")
	}
}
