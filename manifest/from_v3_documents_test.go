package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// documentsManifestJSON declares an Order model plus two printable documents
// (an A4 delivery note and an 80mm POS ticket). It exercises the documents
// contract end-to-end: parse → validate → FromV3 projects contributions.
// documents[] onto the host Manifest.Documents.
const documentsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "POS", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Order",
      "table": "orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "folio", "type": "text", "not_null": true }
      ]
    }
  ],
  "contributions": {
    "documents": [
      { "key": "remision", "model": "Order", "template": "templates/remision.html", "paper": "A4", "filename": "remision-{{record.folio}}" },
      { "key": "ticket", "model": "Order", "template": "templates/ticket.html", "paper": "ticket80" }
    ]
  }
}`

func TestV3Parse_AcceptsDocuments(t *testing.T) {
	m, err := v3.Parse([]byte(documentsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	if m.Contributions == nil || len(m.Contributions.Documents) != 2 {
		t.Fatalf("expected 2 documents, got %+v", m.Contributions)
	}
	if m.Contributions.Documents[0].Key != "remision" || m.Contributions.Documents[0].Paper != "A4" {
		t.Fatalf("first document not parsed: %+v", m.Contributions.Documents[0])
	}
}

func TestFromV3_ProjectsDocumentsToHostManifest(t *testing.T) {
	m, err := v3.Parse([]byte(documentsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)
	if len(host.Documents) != 2 {
		t.Fatalf("expected 2 host documents, got %d", len(host.Documents))
	}
	remision := host.Documents[0]
	if remision.Key != "remision" || remision.Model != "Order" ||
		remision.Template != "templates/remision.html" || remision.Paper != "A4" ||
		remision.Filename != "remision-{{record.folio}}" {
		t.Fatalf("remision projection wrong: %+v", remision)
	}
	if host.Documents[1].Paper != "ticket80" {
		t.Fatalf("ticket paper wrong: %+v", host.Documents[1])
	}
	// The projected host manifest must pass the strict/install-surface validator.
	if err := host.Validate("3.5.0"); err != nil {
		t.Fatalf("host manifest failed strict validate: %v", err)
	}
}

func TestHostValidate_RejectsDocumentUnknownModel(t *testing.T) {
	m := manifest.Manifest{
		Key:     "pos",
		Name:    "POS",
		Version: "0.1.0",
		Documents: []manifest.DocumentDef{
			{Key: "x", Model: "Ghost", Template: "x.html", Paper: "A4"},
		},
	}
	if err := m.Validate(""); err == nil {
		t.Fatal("expected strict validate to reject unknown document model")
	}
}
