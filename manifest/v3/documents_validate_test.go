package v3

import (
	"strings"
	"testing"
)

// withDocuments returns a baseValid manifest that owns a single model and
// carries the supplied contributions.documents[] entries.
func withDocuments(docs []interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Order",
			"table": "orders",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "folio", "type": "text"},
			},
		},
	}
	m["contributions"] = map[string]interface{}{
		"documents": docs,
	}
	return m
}

func TestDocuments_Valid(t *testing.T) {
	m := withDocuments([]interface{}{
		map[string]interface{}{
			"key":      "remision",
			"model":    "Order",
			"template": "templates/remision.html",
			"paper":    "A4",
			"filename": "remision-{{record.folio}}",
		},
		map[string]interface{}{
			"key":      "ticket",
			"model":    "Order",
			"template": "templates/ticket.html",
			"paper":    "ticket80",
		},
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestDocuments_ValidForExtendedModel(t *testing.T) {
	// A document may bind to a model the addon extends (owned elsewhere).
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Order",
			"table": "orders",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
			},
			"extensions": []interface{}{
				map[string]interface{}{
					"target_model": "sales.Invoice",
					"columns": []interface{}{
						map[string]interface{}{"name": "note", "type": "text"},
					},
				},
			},
		},
	}
	m["contributions"] = map[string]interface{}{
		"documents": []interface{}{
			map[string]interface{}{
				"key": "cfdi", "model": "sales.Invoice",
				"template": "templates/cfdi.html", "paper": "letter",
			},
		},
	}
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestDocuments_RejectsUnknownModel(t *testing.T) {
	m := withDocuments([]interface{}{
		map[string]interface{}{
			"key": "x", "model": "Ghost",
			"template": "templates/x.html", "paper": "A4",
		},
	})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "is not a model of this addon") {
		t.Fatalf("expected unknown-model error, got: %v", err)
	}
}

func TestDocuments_RejectsBadPaper(t *testing.T) {
	m := withDocuments([]interface{}{
		map[string]interface{}{
			"key": "x", "model": "Order",
			"template": "templates/x.html", "paper": "A3",
		},
	})
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected paper enum error, got nil")
	}
}

func TestDocuments_RejectsNonHTMLTemplate(t *testing.T) {
	m := withDocuments([]interface{}{
		map[string]interface{}{
			"key": "x", "model": "Order",
			"template": "templates/x.pdf", "paper": "A4",
		},
	})
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected .html template error, got nil")
	}
}

func TestDocuments_RejectsDuplicateKey(t *testing.T) {
	m := withDocuments([]interface{}{
		map[string]interface{}{"key": "d", "model": "Order", "template": "a.html", "paper": "A4"},
		map[string]interface{}{"key": "d", "model": "Order", "template": "b.html", "paper": "A4"},
	})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate-key error, got: %v", err)
	}
}
