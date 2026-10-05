package modelbase

import (
	"bytes"
	"encoding/json"
)

// This file holds the served shapes of two SDK-facing UI contracts that the
// runtime-react SDK (>= 47.x) consumes from the table metadata and from field
// definitions. Both are pure UI metadata, opt-in and retro-compatible: a model
// that declares neither serves exactly the payload it served before. The JSON
// tags are load-bearing and MUST match packages/runtime-react/src/types.ts
// (DocumentFormsManifest) and option-filter.ts (OptionFilterRule).
//
// See docs/document-forms.md for the contract.

// DocumentForms is the guided create flow of a document-like model (invoice,
// credit note, payment receipt...). When Types is non-empty the SDK replaces
// the generic "Crear" modal with one card per document type; picking a card
// shows that type's own fields and, when the type declares Lines, a line-items
// step. It is served as TableMetadata.document_forms.
type DocumentForms struct {
	// TypeField is the model column that discriminates the document type
	// (e.g. "type"). The SDK writes the picked type's value into it on create.
	// Empty = the type is not written.
	TypeField string `json:"type_field,omitempty"`
	// LinesField is the default payload field that receives the line items
	// (a type may override it via Lines.Field). Empty = "lines" in the SDK.
	LinesField string `json:"lines_field,omitempty"`
	// Types are the selectable document types, in display order.
	Types []DocumentFormType `json:"types"`
}

// DocumentFormType is one selectable document type.
type DocumentFormType struct {
	// Key identifies the type within the model (unique).
	Key string `json:"key"`
	// Label is the card title (literal or i18n key).
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	// Value is what the SDK writes into DocumentForms.TypeField (e.g. "E").
	// Empty = Key.
	Value string `json:"value,omitempty"`
	// Fields are the type's header fields, in the action-field vocabulary.
	Fields []FieldDef `json:"fields"`
	// Defaults are fixed values that travel with this type's create payload.
	Defaults map[string]any `json:"defaults,omitempty"`
	// Lines, when set, gives the type a line-items step. Served as `true` when
	// it carries no option, as an object otherwise (the SDK accepts both).
	Lines *DocumentFormLines `json:"lines,omitempty"`
	// Endpoint overrides the create endpoint for this type. Empty = the model's.
	Endpoint string `json:"endpoint,omitempty"`
	// SubmitLabel is the final button text. Empty = the SDK default.
	SubmitLabel string `json:"submit_label,omitempty"`

	// Layout is "wizard" (default, card → fields → lines) or "editor" (one
	// screen with party card, lines, live totals and preview).
	Layout string `json:"layout,omitempty"`
	// Party is the counterparty card (customer / supplier) of the editor.
	Party *DocumentFormParty `json:"party,omitempty"`
	// Sources are the «Cargar desde…» documents whose lines are copied.
	Sources []DocumentFormSource `json:"sources,omitempty"`
	// Preview names the analyze-only row action that renders the preview.
	Preview *DocumentFormPreview `json:"preview,omitempty"`
	// SubmitAction saves through a create-placed model action instead of the
	// generic create. Empty = the generic create (or Endpoint).
	SubmitAction string `json:"submit_action,omitempty"`
}

// DocumentFormParty names the header field that picks the counterparty and
// what the editor's party card shows.
type DocumentFormParty struct {
	Field    string            `json:"field"`
	Model    string            `json:"model"`
	Endpoint string            `json:"endpoint,omitempty"`
	Summary  []string          `json:"summary,omitempty"`
	Credit   map[string]string `json:"credit,omitempty"`
}

// DocumentFormSource is one «Cargar desde…» origin document.
type DocumentFormSource struct {
	Key           string            `json:"key"`
	Label         string            `json:"label"`
	Model         string            `json:"model"`
	Lines         string            `json:"lines"`
	Map           map[string]string `json:"map,omitempty"`
	Header        map[string]string `json:"header,omitempty"`
	LinkField     string            `json:"link_field,omitempty"`
	OptionFilter  OptionFilter      `json:"option_filter,omitempty"`
	RequiresAddon string            `json:"requires_addon,omitempty"`
}

// DocumentFormPreview is the row action called with analyze=true on the draft.
type DocumentFormPreview struct {
	Action        string `json:"action"`
	RequiresAddon string `json:"requires_addon,omitempty"`
	Label         string `json:"label,omitempty"`
}

// DocumentFormOpenDocuments feeds the payment allocator (lines.kind =
// "allocation"): which open documents of the party can receive an amount.
type DocumentFormOpenDocuments struct {
	Model             string       `json:"model"`
	PartyField        string       `json:"party_field"`
	BalanceField      string       `json:"balance_field"`
	NumberField       string       `json:"number_field"`
	TotalField        string       `json:"total_field,omitempty"`
	DueField          string       `json:"due_field,omitempty"`
	IssuedField       string       `json:"issued_field,omitempty"`
	MethodField       string       `json:"method_field,omitempty"`
	LineDocumentField string       `json:"line_document_field"`
	LineAmountField   string       `json:"line_amount_field"`
	OptionFilter      OptionFilter `json:"option_filter,omitempty"`
}

// DocumentFormLines configures the line-items step of a document type.
type DocumentFormLines struct {
	// Field is the payload field that receives the lines. Empty = the
	// DocumentForms.LinesField (or "lines").
	Field string `json:"field,omitempty"`
	// Columns are the optional editor columns ("discount", "tax", "unit"...).
	Columns []string `json:"columns,omitempty"`
	// PriceSource is "sale" (list price, default) or "cost" (purchases).
	PriceSource string `json:"price_source,omitempty"`
	// Required is whether at least one line is needed to save. Nil = true.
	Required *bool `json:"required,omitempty"`
	// Title is the step heading. Empty = the SDK default ("Renglones").
	Title string `json:"title,omitempty"`
	// DiscountMode is "percent" (default), "amount" or "both".
	DiscountMode string `json:"discount_mode,omitempty"`
	// Kind is sale (default) | purchase | credit | allocation | workorder.
	Kind string `json:"kind,omitempty"`
	// OpenDocuments configures the allocator when Kind = "allocation".
	OpenDocuments *DocumentFormOpenDocuments `json:"open_documents,omitempty"`
}

// isZero reports whether the lines step carries no option at all.
func (l DocumentFormLines) isZero() bool {
	return l.Field == "" && len(l.Columns) == 0 && l.PriceSource == "" && l.Required == nil && l.Title == "" &&
		l.DiscountMode == "" && l.Kind == "" && l.OpenDocuments == nil
}

type documentFormLinesAlias DocumentFormLines

// MarshalJSON serves an option-less lines step as `true` (the SDK's boolean
// form) and an object otherwise.
func (l DocumentFormLines) MarshalJSON() ([]byte, error) {
	if l.isZero() {
		return []byte("true"), nil
	}
	return json.Marshal(documentFormLinesAlias(l))
}

// UnmarshalJSON accepts `true` (default lines step) or the object form. A
// `false` decodes to the zero value too; callers that need to distinguish it
// (the manifest path) drop it before serving.
func (l *DocumentFormLines) UnmarshalJSON(data []byte) error {
	t := bytes.TrimSpace(data)
	if bytes.Equal(t, []byte("true")) || bytes.Equal(t, []byte("false")) || bytes.Equal(t, []byte("null")) {
		*l = DocumentFormLines{}
		return nil
	}
	var a documentFormLinesAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*l = DocumentFormLines(a)
	return nil
}

// HasDocumentForms is implemented by models (compiled Go models, or the host's
// definer for an addon model) that serve a guided document create flow.
// metadata.Service projects it onto TableMetadata.DocumentForms when the model
// did not already set it in DefineTable.
type HasDocumentForms interface {
	DefineDocumentForms() *DocumentForms
}

// OptionFilterRule hides options of a relation / dynamic picker. The SDK
// evaluates it client-side over the extra columns the /options endpoint
// returns (FieldOptionsConfig.ExtraColumns): Field names an option property
// (a returned extra column such as "status", or id/value/label/name/
// description/color/icon). Comparison is trimmed and case-insensitive.
// Positive rules (Equals, In) need the property to exist; negative rules
// (NotEquals, NotIn) keep an option that lacks it. Values are string, number
// or boolean.
type OptionFilterRule struct {
	Field     string `json:"field"`
	Equals    any    `json:"equals,omitempty"`
	NotEquals any    `json:"not_equals,omitempty"`
	In        []any  `json:"in,omitempty"`
	NotIn     []any  `json:"not_in,omitempty"`
}

// OptionFilter is one rule or a list of rules that must ALL pass (AND). It is
// always served as a list; it is accepted as either an object or a list.
type OptionFilter []OptionFilterRule

// UnmarshalJSON accepts a single rule object or an array of rules.
func (f *OptionFilter) UnmarshalJSON(data []byte) error {
	t := bytes.TrimSpace(data)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		*f = nil
		return nil
	}
	if t[0] == '{' {
		var r OptionFilterRule
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		*f = OptionFilter{r}
		return nil
	}
	var list []OptionFilterRule
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*f = list
	return nil
}
