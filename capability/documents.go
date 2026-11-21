package capability

import "time"

// DocumentsEnrich is the printable-document enrichment contract. A region
// pack (a country's fiscal documents, a regional receipt, …) provides it so the host's document renderer
// holds no country-specific code: the host loads the record and its children,
// asks the provider by doc_kind, merges the answer and hydrates the template.
// Without a provider the host renders the generic document.
const DocumentsEnrich = "documents.enrich"

// DocumentsEnrichTimeout is the documents.enrich deadline per provider: a
// pure transformation of rows the host already loaded plus a few id lookups,
// on the print path.
const DocumentsEnrichTimeout = 5 * time.Second

// Error codes documents.enrich providers report (NewCodedError).
const (
	// ErrCodeNotApplicable: the provider does not enrich this doc_kind (or
	// this record). Not a failure: the host renders the generic document.
	ErrCodeNotApplicable = "not_applicable"
)

func init() {
	for _, c := range documentContracts() {
		mustRegister(c)
	}
}

func documentContracts() []Contract {
	return []Contract{
		{
			Key: DocumentsEnrich, Kind: KindQuery, Cardinality: One, Timeout: DocumentsEnrichTimeout,
			Doc: "enrich one printable document (record + children) before its template is hydrated",
			Input: schema(
				req("doc_kind", TypeString, "DocumentDef.kind of the document (snake_case, e.g. invoice, credit_note, payment_receipt); a provider that does not know it answers not_applicable"),
				req("model", TypeString, "model key of the record being printed"),
				opt("document_key", TypeString, "DocumentDef.key of the document"),
				req("record", TypeObject, "the record as the host loaded it (column -> value)"),
				opt("children", TypeObject, "preloaded child relations: name -> array of rows"),
				opt("branding", TypeObject, "org branding fields (string -> string)"),
				opt("locale", TypeString, "BCP 47 language tag of the print (en, es, pt-BR)"),
			),
			Output: schema(
				opt("record", TypeObject, "fields to merge over the record (a patch: absent keys stay); the provider never removes columns"),
				opt("children", TypeObject, "child sets to REPLACE by name (name -> array of rows); absent names stay"),
				opt("branding", TypeObject, "branding fields to merge over the org branding for THIS print (string -> string); e.g. the emisor a stamped comprobante names"),
				opt("line_items", TypeArray, "the main line-item rows for {{#line_items}}; omitted = the host's own"),
			),
			Errors: []string{ErrCodeInvalidInput, ErrCodeNotApplicable},
		},
	}
}
