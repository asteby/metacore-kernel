package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// publicRoutesManifestJSON declares a Quote model with a public_token column,
// a printable document and two public routes: the document-kind route the
// quotes addon ships ({key, model, token_column, kind, document}) and a json
// tracking route with allowlist + expiry + enabled_when. It exercises the
// contract end-to-end: parse → validate → FromV3 carries
// contributions.public_routes[] onto the host Manifest.PublicRoutes.
const publicRoutesManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "quotes", "name": "Quotes", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Quote",
      "table": "quotes",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "folio", "type": "text", "not_null": true },
        { "name": "status", "type": "text" },
        { "name": "total", "type": "numeric(12,2)" },
        { "name": "public_token", "type": "text" },
        { "name": "valid_until", "type": "date" }
      ],
      "relations": [
        { "name": "items", "kind": "one_to_many", "through": "QuoteItem", "foreign_key": "quote_id" }
      ]
    },
    {
      "key": "QuoteItem",
      "table": "quote_items",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "quote_id", "type": "uuid" },
        { "name": "description", "type": "text" }
      ]
    }
  ],
  "contributions": {
    "documents": [
      { "key": "quote", "model": "Quote", "template": "templates/quote.html", "paper": "A4" }
    ],
    "public_routes": [
      { "key": "quote_public", "model": "Quote", "token_column": "public_token", "kind": "document", "document": "quote" },
      { "key": "quote_tracking", "model": "Quote", "token_column": "public_token", "kind": "json",
        "columns": ["folio", "status", "total"], "relations": ["items"],
        "expires_column": "valid_until", "label": "quotes.public.tracking",
        "enabled_when": "status != 'draft'" }
    ]
  }
}`

func TestV3Parse_AcceptsPublicRoutes(t *testing.T) {
	if err := v3.Validate([]byte(publicRoutesManifestJSON)); err != nil {
		t.Fatalf("v3.Validate: %v", err)
	}
	m, err := v3.Parse([]byte(publicRoutesManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	if m.Contributions == nil || len(m.Contributions.PublicRoutes) != 2 {
		t.Fatalf("expected 2 public routes, got %+v", m.Contributions)
	}
	if m.Contributions.PublicRoutes[0].Key != "quote_public" || m.Contributions.PublicRoutes[0].Document != "quote" {
		t.Fatalf("first route not parsed: %+v", m.Contributions.PublicRoutes[0])
	}
}

func TestFromV3_CarriesPublicRoutesToHostManifest(t *testing.T) {
	m, err := v3.Parse([]byte(publicRoutesManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)
	if len(host.PublicRoutes) != 2 {
		t.Fatalf("expected 2 host public routes, got %d", len(host.PublicRoutes))
	}
	tracking := host.PublicRoutes[1]
	if tracking.Key != "quote_tracking" || tracking.Model != "Quote" || tracking.TokenColumn != "public_token" ||
		tracking.Kind != "json" || strings.Join(tracking.Columns, ",") != "folio,status,total" ||
		strings.Join(tracking.Relations, ",") != "items" || tracking.ExpiresColumn != "valid_until" ||
		tracking.Label != "quotes.public.tracking" || tracking.EnabledWhen != "status != 'draft'" {
		t.Fatalf("tracking projection wrong: %+v", tracking)
	}
	// The projected host manifest must pass the strict/install-surface validator.
	if err := host.Validate("3.5.0"); err != nil {
		t.Fatalf("host manifest failed strict validate: %v", err)
	}
	// And the wire form (what /metacore/manifests serves) must carry the field
	// under its documented JSON name, round-tripping unchanged.
	raw, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		PublicRoutes []v3.PublicRoute `json:"public_routes"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.PublicRoutes) != 2 || wire.PublicRoutes[1].EnabledWhen != "status != 'draft'" {
		t.Fatalf("wire form lost public_routes: %s", raw)
	}
}

func TestFromV3_NoPublicRoutesStaysNil(t *testing.T) {
	m, err := v3.Parse([]byte(documentsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)
	if host.PublicRoutes != nil {
		t.Fatalf("expected nil public routes, got %+v", host.PublicRoutes)
	}
	raw, _ := json.Marshal(host)
	if strings.Contains(string(raw), "public_routes") {
		t.Fatalf("wire form must omit public_routes when empty: %s", raw)
	}
}

func TestHostValidate_PublicRoutes(t *testing.T) {
	base := func() manifest.Manifest {
		return manifest.Manifest{
			Key: "quotes", Name: "Quotes", Version: "0.1.0",
			ModelDefinitions: []manifest.ModelDefinition{{
				ModelKey: "Quote", TableName: "quotes",
				Columns: []manifest.ColumnDef{
					{Name: "id", Type: "uuid"},
					{Name: "organization_id", Type: "uuid"},
					{Name: "folio", Type: "text"},
					{Name: "public_token", Type: "text"},
					{Name: "total", Type: "decimal"},
				},
			}},
			Documents: []manifest.DocumentDef{{Key: "quote", Model: "Quote", Template: "templates/quote.html", Paper: "A4"}},
		}
	}
	ok := base()
	ok.PublicRoutes = []v3.PublicRoute{{Key: "quote_public", Model: "Quote", TokenColumn: "public_token", Kind: "document", Document: "quote"}}
	if err := ok.Validate(""); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}

	cases := []struct {
		name string
		r    v3.PublicRoute
		want string
	}{
		{"unknown model", v3.PublicRoute{Key: "x", Model: "Ghost", TokenColumn: "public_token", Kind: "json", Columns: []string{"folio"}}, "not a model"},
		{"non-text token", v3.PublicRoute{Key: "x", Model: "Quote", TokenColumn: "total", Kind: "json", Columns: []string{"folio"}}, "text column"},
		{"unknown document", v3.PublicRoute{Key: "x", Model: "Quote", TokenColumn: "public_token", Kind: "document", Document: "ghost"}, "documents[] key"},
		{"token exposed", v3.PublicRoute{Key: "x", Model: "Quote", TokenColumn: "public_token", Kind: "json", Columns: []string{"public_token"}}, "token column"},
		{"bad expr", v3.PublicRoute{Key: "x", Model: "Quote", TokenColumn: "public_token", Kind: "json", Columns: []string{"folio"}, EnabledWhen: "status =="}, "enabled_when"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			m.PublicRoutes = []v3.PublicRoute{c.r}
			err := m.Validate("")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected error mentioning %q, got %v", c.want, err)
			}
		})
	}
}
