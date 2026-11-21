package capability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDocumentsEnrichContract(t *testing.T) {
	c, ok := Lookup(DocumentsEnrich)
	if !ok {
		t.Fatal("documents.enrich not registered")
	}
	if !c.IsQuery() || c.Fanout() || c.Timeout != DocumentsEnrichTimeout {
		t.Fatalf("shape: query/one/timeout wrong: %+v", c)
	}
	if got := strings.Join(c.Required, ","); got != "doc_kind,model,record" {
		t.Fatalf("required = %q", got)
	}
	// The host pre-checks the required fields.
	if errs := c.CheckInput(map[string]any{"model": "Invoice"}); len(errs) == 0 {
		t.Fatal("missing doc_kind/record must be rejected")
	}
	ok1 := map[string]any{
		"doc_kind": "cfdi_invoice", "model": "Invoice",
		"record":   map[string]any{"total": "10.00"},
		"children": map[string]any{"line_items": []any{map[string]any{"qty": 1}}},
	}
	if errs := c.CheckInput(ok1); len(errs) != 0 {
		t.Fatalf("valid input rejected: %v", errs)
	}
	if errs := c.CheckInput(map[string]any{"doc_kind": "x", "model": "M", "record": "not-an-object"}); len(errs) == 0 {
		t.Fatal("record must be an object")
	}
}

func TestDocumentsEnrichOutputAdditiveAndClosed(t *testing.T) {
	c := Contracts[DocumentsEnrich]
	good, _ := json.Marshal(map[string]any{
		"record":     map[string]any{"tipo_comprobante": "I - Ingreso"},
		"children":   map[string]any{"payments": []any{}},
		"branding":   map[string]any{"tax_id": "EKU9003173C9"},
		"line_items": []any{map[string]any{"description": "x"}},
	})
	if errs := c.ValidateOutput(good); len(errs) != 0 {
		t.Fatalf("valid output rejected: %v", errs)
	}
	// An empty patch is a legal answer (nothing to enrich).
	if errs := c.ValidateOutput([]byte(`{}`)); len(errs) != 0 {
		t.Fatalf("empty patch rejected: %v", errs)
	}
	// Closed output: an unknown top-level key is refused.
	if errs := c.ValidateOutput([]byte(`{"html":"<b>x</b>"}`)); len(errs) == 0 {
		t.Fatal("unknown output field must be rejected")
	}
	if errs := c.ValidateOutput([]byte(`{"branding":"x"}`)); len(errs) == 0 {
		t.Fatal("branding must be an object")
	}
	if errs := c.ValidateOutput([]byte(`{"line_items":{"a":1}}`)); len(errs) == 0 {
		t.Fatal("line_items must be an array")
	}
}
