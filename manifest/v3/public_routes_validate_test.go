package v3

import (
	"strings"
	"testing"
)

// withPublicRoutes returns a baseValid manifest owning a Quote model (with a
// text public_token, a timestamptz expires_at and a one_to_many `items`
// relation), a QuoteItem child, one printable document bound to Quote, and the
// supplied contributions.public_routes[] entries.
func withPublicRoutes(routes []interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Quote",
			"table": "quotes",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "folio", "type": "text"},
				map[string]interface{}{"name": "status", "type": "text"},
				map[string]interface{}{"name": "total", "type": "numeric(12,2)"},
				map[string]interface{}{"name": "public_token", "type": "text"},
				map[string]interface{}{"name": "short_token", "type": "varchar(64)"},
				map[string]interface{}{"name": "expires_at", "type": "timestamptz"},
				map[string]interface{}{"name": "customer_id", "type": "uuid", "ref": "Customer"},
			},
			"relations": []interface{}{
				map[string]interface{}{"name": "items", "kind": "one_to_many", "through": "QuoteItem", "foreign_key": "quote_id"},
			},
		},
		map[string]interface{}{
			"key":   "QuoteItem",
			"table": "quote_items",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "quote_id", "type": "uuid"},
				map[string]interface{}{"name": "description", "type": "text"},
			},
		},
	}
	m["contributions"] = map[string]interface{}{
		"documents": []interface{}{
			map[string]interface{}{"key": "quote", "model": "Quote", "template": "templates/quote.html", "paper": "A4"},
		},
		"public_routes": routes,
	}
	return m
}

func route(overrides map[string]interface{}) map[string]interface{} {
	r := map[string]interface{}{
		"key":          "quote_public",
		"model":        "Quote",
		"token_column": "public_token",
		"kind":         "document",
		"document":     "quote",
	}
	for k, v := range overrides {
		if v == nil {
			delete(r, k)
			continue
		}
		r[k] = v
	}
	return r
}

func TestPublicRoutes_Valid(t *testing.T) {
	m := withPublicRoutes([]interface{}{
		// The exact shape the quotes addon declares.
		route(nil),
		route(map[string]interface{}{
			"key": "quote_json", "kind": "json",
			"columns":        []interface{}{"folio", "status", "total"},
			"relations":      []interface{}{"items", "customer"},
			"expires_column": "expires_at",
			"label":          "quotes.public.tracking",
			"enabled_when":   "status in ('sent','accepted') && total > 0",
		}),
		route(map[string]interface{}{
			"key": "quote_page", "kind": "html",
			"columns": []interface{}{"folio", "total"},
		}),
		// html with no columns is fine when a document backs the page.
		route(map[string]interface{}{"key": "quote_dl", "kind": "html"}),
		// varchar token columns are textual too.
		route(map[string]interface{}{"key": "quote_short", "token_column": "short_token"}),
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestPublicRoutes_ValidForExtendedModel(t *testing.T) {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key":   "Note",
			"table": "notes",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
			},
			"extensions": []interface{}{
				map[string]interface{}{
					"target_model": "sales.Invoice",
					"columns":      []interface{}{map[string]interface{}{"name": "share_token", "type": "text"}},
				},
			},
		},
	}
	m["contributions"] = map[string]interface{}{
		"public_routes": []interface{}{
			map[string]interface{}{"key": "invoice_page", "model": "sales.Invoice", "token_column": "share_token", "kind": "json", "columns": []interface{}{"folio"}},
		},
	}
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid (column checks skipped for extended models), got: %v", err)
	}
}

func TestPublicRoutes_Rejections(t *testing.T) {
	cases := []struct {
		name string
		r    map[string]interface{}
		want string
	}{
		{"unknown model", route(map[string]interface{}{"model": "Ghost"}), "not a model of this addon"},
		{"missing token column", route(map[string]interface{}{"token_column": "nope"}), "not a column of model"},
		{"non-text token column", route(map[string]interface{}{"token_column": "total"}), "must be a text column"},
		{"bad kind", route(map[string]interface{}{"kind": "pdf"}), "kind"},
		{"document required", route(map[string]interface{}{"document": nil}), "document is required"},
		{"unknown document", route(map[string]interface{}{"document": "ghost"}), "not a contributions.documents[] key"},
		{"json needs columns", route(map[string]interface{}{"kind": "json", "document": nil}), "at least one column"},
		{"html needs columns or document", route(map[string]interface{}{"kind": "html", "document": nil}), "at least one column"},
		{"unknown column", route(map[string]interface{}{"kind": "json", "columns": []interface{}{"ghost"}}), "not a column of model"},
		{"token in columns", route(map[string]interface{}{"kind": "json", "columns": []interface{}{"public_token"}}), "must not expose the token column"},
		{"unknown relation", route(map[string]interface{}{"relations": []interface{}{"ghost"}}), "neither a relation nor a ref column"},
		{"missing expires column", route(map[string]interface{}{"expires_column": "ghost"}), "not a column of model"},
		{"non-temporal expires column", route(map[string]interface{}{"expires_column": "folio"}), "must be a date/timestamp column"},
		{"bad enabled_when", route(map[string]interface{}{"enabled_when": "status == draft"}), "enabled_when"},
		{"enabled_when unknown field", route(map[string]interface{}{"enabled_when": "ghost == 'x'"}), "references \"ghost\""},
		{"bad key pattern", route(map[string]interface{}{"key": "Quote-Public"}), "key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := withPublicRoutes([]interface{}{c.r})
			err := Validate(mustJSON(t, m))
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestPublicRoutes_RejectsDuplicateKey(t *testing.T) {
	m := withPublicRoutes([]interface{}{route(nil), route(nil)})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate-key rejection, got: %v", err)
	}
}

func TestPublicRoutes_DocumentMustBindSameModel(t *testing.T) {
	m := withPublicRoutes([]interface{}{
		route(map[string]interface{}{"model": "QuoteItem", "token_column": "description"}),
	})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "binds model") {
		t.Fatalf("expected model-mismatch rejection, got: %v", err)
	}
}

func TestPublicRoutes_SchemaRejectsUnknownField(t *testing.T) {
	m := withPublicRoutes([]interface{}{route(map[string]interface{}{"bogus": true})})
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected the strict schema to reject an unknown public_routes field")
	}
}

func TestPublicRoutes_ParseCarriesEveryField(t *testing.T) {
	m := withPublicRoutes([]interface{}{
		route(map[string]interface{}{
			"key": "quote_json", "kind": "json",
			"columns":        []interface{}{"folio", "status"},
			"relations":      []interface{}{"items"},
			"expires_column": "expires_at",
			"label":          "quotes.public.tracking",
			"enabled_when":   "status != 'draft'",
		}),
	})
	parsed, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Contributions == nil || len(parsed.Contributions.PublicRoutes) != 1 {
		t.Fatalf("expected 1 public route, got %+v", parsed.Contributions)
	}
	r := parsed.Contributions.PublicRoutes[0]
	if r.Key != "quote_json" || r.Model != "Quote" || r.TokenColumn != "public_token" || r.Kind != "json" ||
		r.Document != "quote" || len(r.Columns) != 2 || len(r.Relations) != 1 || r.ExpiresColumn != "expires_at" ||
		r.Label != "quotes.public.tracking" || r.EnabledWhen != "status != 'draft'" {
		t.Fatalf("public route not parsed faithfully: %+v", r)
	}
}
