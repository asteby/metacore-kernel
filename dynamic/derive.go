package dynamic

import (
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
	kstrings "github.com/asteby/metacore-kernel/strings"
)

// managedFormColumns are the system/managed columns that must never surface as
// editable form fields in a derived create/edit modal. They are owned by the
// kernel's dynamic schema layer (id/created_at/updated_at), the multi-tenant
// scope (organization_id) or the soft-delete plane (deleted_at), so the user
// never sets them by hand. Table columns may still render them (a host can
// choose to show created_at), which is why this set only gates form fields.
var managedFormColumns = map[string]struct{}{
	"id":              {},
	"created_at":      {},
	"updated_at":      {},
	"organization_id": {},
	"deleted_at":      {},
}

// DeriveTableColumns builds default UI table columns from a model definition's
// physical column list. It is meant for addon-installed models that ship no
// explicit `table.columns` UI spec: without derived columns the metadata
// response carries `columns: null` and the SDK dynamic table crashes. Every
// derived column is sortable and gets a widget type mapped from its storage
// type via ColumnUIType.
//
// The function is PURE and host-agnostic: it does NOT localize. Label is a
// humanized form of the raw column name (snake_case → Title Case, trailing
// " Id" stripped) so a host with no i18n still gets readable headers. Hosts
// that own an i18n dictionary should overwrite Label per column after calling
// this (e.g. via a `field.<name>` lookup) — the kernel deliberately keeps no
// translation vocabulary.
func DeriveTableColumns(def manifest.ModelDefinition) []modelbase.ColumnDef {
	out := make([]modelbase.ColumnDef, 0, len(def.Columns))
	for _, c := range def.Columns {
		out = append(out, modelbase.ColumnDef{
			Key:      c.Name,
			Label:    humanizeColumnName(c.Name),
			Type:     ColumnUIType(c.Type),
			Sortable: true,
		})
	}
	return out
}

// DeriveFormFields builds default create/edit form fields from a model
// definition's physical column list, for addon models that ship no modal spec.
// Managed columns (id/created_at/updated_at/organization_id/deleted_at) are
// skipped so the user only edits real attributes. Like DeriveTableColumns it is
// PURE and host-agnostic: Label is humanized, not localized; the host overrides
// it with its own dictionary.
func DeriveFormFields(def manifest.ModelDefinition) []modelbase.FieldDef {
	out := make([]modelbase.FieldDef, 0, len(def.Columns))
	for _, c := range def.Columns {
		if _, managed := managedFormColumns[c.Name]; managed {
			continue
		}
		// Resolve the form field type: an explicit Widget wins (the manifest
		// opted in, e.g. "textarea"/"email"), else map the storage type, then
		// promote known image columns (logo/image/photo/…) to a file picker and
		// long-text columns (description/notes/…) to a textarea.
		ftype := FormFieldType(c.Type)
		if c.Widget != "" {
			ftype = c.Widget
		} else if ftype == "text" && imageColumn(c.Name) {
			ftype = "image"
		} else if ftype == "text" && longTextColumn(c.Name) {
			ftype = "textarea"
		}
		out = append(out, modelbase.FieldDef{
			Key:      c.Name,
			Label:    humanizeColumnName(c.Name),
			Type:     ftype,
			Widget:   c.Widget,
			Required: c.Required,
		})
	}
	return out
}

// ColumnUIType maps a physical/storage column type to the table column display
// type understood by the SDK dynamic table. Unknown types fall back to "text".
func ColumnUIType(storageType string) string {
	switch storageType {
	case "integer", "int", "bigint", "float", "numeric", "decimal", "number":
		return "number"
	case "boolean", "bool":
		return "boolean"
	case "date", "datetime", "timestamp", "timestamptz":
		return "datetime"
	default:
		return "text"
	}
}

// FormFieldType maps a physical/storage column type to the form widget type
// understood by the SDK dynamic form. Unknown types fall back to "text".
func FormFieldType(storageType string) string {
	switch storageType {
	case "integer", "int", "bigint", "float", "numeric", "decimal", "number":
		return "number"
	case "boolean", "bool":
		return "boolean"
	case "date", "datetime", "timestamp", "timestamptz":
		return "date"
	case "jsonb", "json":
		// Structured blobs need the room; a single-line input can't show them.
		return "textarea"
	default:
		// `text`, `varchar`, `string`, … all default to a single-line input.
		// Most text columns (sku, name, email, slug) are short; a textarea for
		// every one of them turns forms into a wall of giant boxes. Long-text
		// fields opt into a textarea via an explicit Widget or the name
		// heuristic applied in DeriveFormFields.
		return "text"
	}
}

// longTextColumn names columns that read better as a multi-line textarea even
// though they're stored as plain `text`. Pure heuristic on the column name so
// addons get sensible forms without hand-authoring a Widget for every field.
func longTextColumn(name string) bool {
	switch strings.ToLower(name) {
	case "description", "notes", "note", "comment", "comments", "body",
		"content", "address", "message", "summary", "bio", "remarks",
		"observations", "details":
		return true
	default:
		return false
	}
}

// imageColumn names columns that hold an image/file URL and should render as a
// file picker (upload → store → save the URL) instead of a raw text input.
// Pure heuristic on the column name (the value is still a `text` URL), so logo/
// image/avatar fields get an uploader without hand-authoring a Widget.
func imageColumn(name string) bool {
	switch strings.ToLower(name) {
	case "logo", "image", "image_url", "photo", "picture", "avatar", "icon",
		"banner", "cover", "thumbnail", "thumb", "logo_url", "avatar_url":
		return true
	default:
		return false
	}
}

// DefaultsCRUDEnabled documents the kernel's stance on CRUD-on-by-default: a
// derived (addon) model with physical columns but no hand-authored actions
// should expose the standard create/edit/view/delete affordances, otherwise its
// records can never be managed from the UI. The actual wiring of CRUD action
// buttons is host-specific (the action shapes, icons and i18n live in the
// host), so the kernel only reports the recommendation; the host decides.
func DefaultsCRUDEnabled(def manifest.ModelDefinition) bool {
	return len(def.Columns) > 0
}

// humanizeColumnName turns a raw snake_case column name into a readable Title
// Case label, dropping a trailing " Id" so foreign-key columns like
// `customer_id` read as "Customer" rather than "Customer Id". This is the
// host-agnostic fallback; a host with i18n overrides it.
func humanizeColumnName(name string) string {
	humanized := kstrings.TitleCase(strings.ReplaceAll(name, "_", " "))
	return strings.TrimSuffix(humanized, " Id")
}
