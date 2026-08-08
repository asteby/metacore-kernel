package modelbase

import (
	"fmt"
	"strings"
)

// This file holds the spreadsheet-import contract. It is the single source of
// truth for BOTH ends of the flow: the generated template (headers, example
// row, hints) and the parser that reads a filled-in file back. Keeping one
// declaration for both is what stops the two halves from drifting — a column
// renamed in the template cannot silently stop matching on the way in.
//
// A model declares nothing by default: DeriveImportSpec projects a usable spec
// from the model's ModalMetadata, so every registered model gets a working
// import without per-app wiring. Models with domain rules the form does not
// express (composite dot-paths, generated values, friendlier aliases) override
// it by implementing HasImportSpec.

// ImportColumn is one spreadsheet column of an import template.
type ImportColumn struct {
	// Key is the input path written into the record passed to Service.Create.
	// Dot-paths ("user.email") address nested/related structures the same way
	// the dynamic create pipeline already does.
	Key string `json:"key"`
	// Header is the human column title as it appears in the template. It is
	// what the user sees, so it is also the primary match on the way back in.
	Header string `json:"header"`
	// Aliases are additional headers accepted when parsing. Matching is
	// case-insensitive and ignores a trailing "*" required-marker, so those
	// variants need not be listed. Use this for synonyms a user may type by
	// hand ("Correo" for "Email") or headers kept for backwards compatibility.
	Aliases []string `json:"aliases,omitempty"`
	// Required rejects the row when the cell is empty. The template renders
	// these headers with a trailing "*".
	Required bool `json:"required,omitempty"`
	// Type mirrors FieldDef.Type and drives validation of the cell value.
	Type string `json:"type,omitempty"`
	// Example fills the sample row of the generated template.
	Example string `json:"example,omitempty"`
	// Hint is the short description rendered under the example row.
	Hint string `json:"hint,omitempty"`
	// Generator names a value provider used when the cell is left empty (e.g.
	// "random_password"). Generators are registered by the host; an unknown
	// name leaves the value absent rather than failing the row.
	Generator string `json:"generator,omitempty"`
	// Transform names a cell post-processor run after coercion (e.g.
	// "media_url" fetches a remote image into host storage). Transforms are
	// registered on the importer package; an unknown name fails the row.
	Transform string `json:"transform,omitempty"`
}

// ImportSpec is a model's full spreadsheet-import declaration.
type ImportSpec struct {
	Columns []ImportColumn `json:"columns"`
	// MaxRows caps a single upload. Zero means DefaultImportMaxRows.
	MaxRows int `json:"maxRows,omitempty"`
	// SheetName titles the data sheet of the generated XLSX. Empty defaults to
	// the model's table title.
	SheetName string `json:"sheetName,omitempty"`
	// Instructions are free-form lines rendered on a second sheet of the
	// template. Use them to state what the import does NOT cover (relations
	// assigned later, credential handling, error-retry flow).
	Instructions []string `json:"instructions,omitempty"`
	// Defaults are values written into EVERY imported record without appearing
	// as a column. They carry what the create form supplies invisibly — a role,
	// a discriminator, a status — which the model still requires. Without them
	// an imported row is subtly different from one created through the form
	// (e.g. a patient with an empty role), and nothing in the file hints at it.
	Defaults map[string]any `json:"defaults,omitempty"`
}

// DefaultImportMaxRows caps a single upload when the spec does not say. It is
// deliberately low enough that a synchronous import stays within a request
// timeout; bigger datasets belong on an async path.
const DefaultImportMaxRows = 1000

// HasImportSpec is implemented by models that override the derived spec.
type HasImportSpec interface {
	DefineImport() ImportSpec
}

// ImportRecordNormalizer is the host/custom hook for row reshaping after the
// engine has coerced cells (and run transforms) but BEFORE create. Use it for
// domain composition that is not a single-cell transform — e.g. joining
// prefix+given+last into `user.name`, or building a birth date from D/M/Y
// columns. Any MetaCore host or addon model can implement this; the kernel
// never hard-codes domain rules here.
type ImportRecordNormalizer interface {
	NormalizeImportRecord(record map[string]any) error
}

// ImportRelationsAttacher is the host/custom hook run AFTER a successful
// create. Use it for M2M links, gallery rows, or anything that needs the new
// primary key. Keep the kernel free of per-product relation graphs.
type ImportRelationsAttacher interface {
	// DB is intentionally `any` so modelbase stays free of a GORM dependency;
	// hosts pass *gorm.DB (or their own unit-of-work) and cast inside.
	AttachImportRelations(db any, record map[string]any, createdID uint) error
}

// importableFieldTypes lists the form field types that survive a round-trip
// through a spreadsheet cell. Binary and composite widgets are dropped from a
// derived spec: a user cannot paste an image into a CSV, and silently
// accepting the column would produce rows that always fail.
var importableFieldTypes = map[string]bool{
	"":         true, // untyped defaults to text
	"text":     true,
	"textarea": true,
	"email":    true,
	"url":      true,
	"number":   true,
	"date":     true,
	"boolean":  true,
	"select":   true,
}

// DefaultSecretGenerator names the generator attached to derived password
// columns. A password is REQUIRED by the model yet must never be dictated by a
// spreadsheet author for every row, so the derived template offers the column
// (an admin may set one deliberately) and fills a blank cell with an
// unguessable value the user then resets through the normal recovery flow.
const DefaultSecretGenerator = "random_secret"

// DeriveImportSpec projects a default ImportSpec from a model's form metadata,
// so a model that declares nothing still gets a template whose headers match
// its own form labels. Non-importable field types (image, file, relation
// pickers) are skipped.
func DeriveImportSpec(modal ModalMetadata) ImportSpec {
	cols := make([]ImportColumn, 0, len(modal.Fields))
	defaults := map[string]any{}
	for _, f := range modal.Fields {
		// A hidden field is filled by the form itself (a role, a discriminator).
		// It is not a column — the user has nothing to type — but its default
		// MUST still reach the record, or an imported row differs from one
		// created through the form in a way nobody can see in the file.
		if f.Type == "hidden" {
			if f.DefaultValue != nil {
				defaults[f.Key] = f.DefaultValue
			}
			continue
		}
		// A password is required but must not be dictated per row: it becomes a
		// column with a generator, so a blank cell yields an unguessable value.
		if f.Type == "password" {
			header := f.Label
			if header == "" {
				header = f.Key
			}
			cols = append(cols, ImportColumn{
				Key:       f.Key,
				Header:    header,
				Generator: DefaultSecretGenerator,
				Hint:      f.Placeholder,
			})
			continue
		}
		if !importableFieldTypes[f.Type] {
			// A REQUIRED field that cannot travel in a spreadsheet cell makes
			// the whole derived template useless: every row would be missing a
			// value the model demands (e.g. an appointment's doctor and patient
			// pickers). Offering an import that cannot succeed is worse than
			// offering none, so the model opts out of the derived path — it can
			// still declare DefineImport with the dot-paths it wants.
			if f.Required {
				return ImportSpec{}
			}
			continue
		}
		header := f.Label
		if header == "" {
			header = f.Key
		}
		col := ImportColumn{
			Key:      f.Key,
			Header:   header,
			Required: f.Required,
			Type:     f.Type,
			Hint:     f.Placeholder,
		}
		// The field key is always accepted as an alias so a file exported by
		// the raw CSV export (which uses keys, not labels) feeds straight back
		// in without renaming a single header.
		if !strings.EqualFold(f.Key, header) {
			col.Aliases = append(col.Aliases, f.Key)
		}
		cols = append(cols, col)
	}
	spec := ImportSpec{Columns: cols}
	if len(defaults) > 0 {
		spec.Defaults = defaults
	}
	return spec
}

// StaticImportSpec makes a fixed spec satisfy HasImportSpec by embedding. It
// exists for the ADDON path: a host that synthesises a ModelDefiner from a
// manifest has no Go method to hang DefineImport on, so it embeds this with
// the spec decoded from the manifest's `import` block. Both kinds of model —
// compiled struct and manifest-declared — then resolve identically through
// ResolveImportSpec, which is what keeps one import engine serving both.
type StaticImportSpec struct {
	Spec ImportSpec
}

// DefineImport implements HasImportSpec.
func (s StaticImportSpec) DefineImport() ImportSpec { return s.Spec }

// ResolveImportSpec returns the model's own spec when it declares one, and the
// spec derived from its form otherwise. Callers should use this rather than
// type-asserting HasImportSpec themselves.
func ResolveImportSpec(model any, modal ModalMetadata) ImportSpec {
	if d, ok := model.(HasImportSpec); ok {
		spec := d.DefineImport()
		if len(spec.Columns) > 0 {
			return spec
		}
	}
	return DeriveImportSpec(modal)
}

// Limit returns the effective row cap for the spec.
func (s ImportSpec) Limit() int {
	if s.MaxRows > 0 {
		return s.MaxRows
	}
	return DefaultImportMaxRows
}

// TemplateHeader renders the column title as written into the template file,
// with the "*" marker appended for required columns.
func (c ImportColumn) TemplateHeader() string {
	if c.Required {
		return c.Header + " *"
	}
	return c.Header
}

// normalizeHeader canonicalises a header for matching: case-folded, trimmed,
// and stripped of the required-marker the template appends.
func normalizeHeader(h string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(h), "*")))
}

// HeaderIndex maps every accepted spelling of every column to that column, so
// a parser can resolve an arbitrary uploaded header row in one pass.
//
// FIRST declaration wins on a collision. Two columns whose headers normalise to
// the same string (or an alias shadowing another column's header) is an authoring
// mistake that would otherwise route a user's data into the wrong field, so the
// order is defined rather than left to map iteration. Validate reports the
// collision so it is caught before the spec ships.
func (s ImportSpec) HeaderIndex() map[string]ImportColumn {
	idx := make(map[string]ImportColumn, len(s.Columns)*2)
	put := func(key string, col ImportColumn) {
		if key == "" {
			return
		}
		if _, taken := idx[key]; taken {
			return
		}
		idx[key] = col
	}
	for _, col := range s.Columns {
		put(normalizeHeader(col.Header), col)
		put(normalizeHeader(col.Key), col)
		for _, alias := range col.Aliases {
			put(normalizeHeader(alias), col)
		}
	}
	return idx
}

// Validate reports authoring mistakes in a spec: a column with no key, and two
// spellings that collide after normalisation. It is not called on the hot path
// — hosts run it when registering models, and the manifest validator runs it on
// an addon's declared block, so a broken spec surfaces at install time instead
// of as data landing in the wrong column.
func (s ImportSpec) Validate() []error {
	var errs []error
	owner := map[string]string{} // normalized spelling → column key that claimed it
	claim := func(spelling, kind string, col ImportColumn) {
		key := normalizeHeader(spelling)
		if key == "" {
			return
		}
		if prev, taken := owner[key]; taken && prev != col.Key {
			errs = append(errs, fmt.Errorf(
				"import spec: %s %q of column %q collides with column %q; a file using it would be routed to the first declaration",
				kind, spelling, col.Key, prev))
			return
		}
		owner[key] = col.Key
	}
	for _, col := range s.Columns {
		if col.Key == "" {
			errs = append(errs, fmt.Errorf("import spec: column %q has no key", col.Header))
			continue
		}
		claim(col.Header, "header", col)
		claim(col.Key, "key", col)
		for _, alias := range col.Aliases {
			claim(alias, "alias", col)
		}
	}
	return errs
}
