package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

func docWithKind(kind string) []interface{} {
	return []interface{}{map[string]interface{}{
		"key": "invoice_print", "model": "Order", "template": "templates/invoice.html", "paper": "letter",
		"kind": kind,
	}}
}

// DocumentDef.kind is optional and additive: valid snake_case passes the
// schema and the Go validator; anything else is rejected by both (dual
// validation), and a manifest without kind is unchanged.
func TestDocuments_KindValid(t *testing.T) {
	if err := Validate(mustJSON(t, withDocuments(docWithKind("invoice")))); err != nil {
		t.Fatalf("valid kind rejected: %v", err)
	}
}

func TestDocuments_KindInvalidRejected(t *testing.T) {
	for _, k := range []string{"INVOICE", "invoice-print", "1invoice", "invoice print"} {
		err := Validate(mustJSON(t, withDocuments(docWithKind(k))))
		if err == nil {
			t.Fatalf("kind %q must be rejected", k)
		}
	}
	// Go validator alone (not only the schema).
	var m Manifest
	if err := json.Unmarshal(mustJSON(t, withDocuments(docWithKind("OK"))), &m); err != nil {
		t.Fatal(err)
	}
	if errs := validateDocuments(&m, map[string]map[string]struct{}{"Order": {}}); len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), "kind") {
		t.Fatalf("Go validator did not flag kind: %v", errs)
	}
}
