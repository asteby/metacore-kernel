package dynamic

import (
	"sort"
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
// The function is PURE and host-agnostic: it does NOT itself translate. Label
// resolution follows a two-stage contract:
//
//  1. DERIVE (here): the served ColumnDef.Label is the column's DECLARED label
//     when the manifest author set one (manifest.ColumnDef.Label, carried from
//     v3 Column.label), otherwise a humanized form of the raw column name
//     (snake_case → Title Case, trailing " Id" stripped). So a host with no
//     i18n still gets readable headers, and an author-declared label is never
//     thrown away.
//  2. LOCALIZE (host, optional): when the declared label is an i18n KEY (it
//     starts with the host's configured prefix, e.g. "models.") the host's
//     metadata.NewLocalizedTableTransformer rewrites it to the browsed locale
//     at serve time via the registered Translator. A plain (non-prefixed)
//     label passes through untouched. The kernel deliberately keeps no
//     translation vocabulary; it only preserves the key so the transformer
//     can resolve it instead of the SDK rendering the raw key.
//
// Hosts that adopt a different i18n convention can still overwrite Label per
// column after calling this; the derivation only sets a sensible default.
func DeriveTableColumns(def manifest.ModelDefinition) []modelbase.ColumnDef {
	out := make([]modelbase.ColumnDef, 0, len(def.Columns))
	for _, c := range def.Columns {
		col := modelbase.ColumnDef{
			Key:      c.Name,
			Label:    columnLabel(c),
			Type:     ColumnUIType(c.Type),
			Sortable: true,
			// Display hints declared on the column (manifest authors) ride
			// straight through to the served metadata.
			CellStyle:   c.CellStyle,
			StyleConfig: c.StyleConfig,
			Tooltip:     c.Tooltip,
			Description: c.Description,
			BasePath:    c.BasePath,
			// Ref (FK target → dynamic_select) and Options (static select)
			// ride straight through from the declared manifest column onto the
			// served metadata so the SDK renders a relation picker / plain
			// select without any per-app wiring or belongs_to auto-derivation.
			Ref:        c.Ref,
			Options:    toOptionDefs(c.Options),
			UseOptions: len(c.Options) > 0,
			// OptionsSource (dynamic provider key) rides through so the HOST
			// can materialise the provider's localized options onto the served
			// column at metadata-serve time. The kernel only carries the key.
			OptionsSource: c.OptionsSource,
			// DependsOn + OptionsConfig forward the dependent-picker declaration
			// (cascade scope + dynamic source) so the SDK re-fetches on change and
			// the host's OptionsConfigResolver can build the runtime query.
			DependsOn:     c.DependsOn,
			OptionsConfig: toFieldOptionsConfig(c.OptionsConfig),
			// Scan carries the camera scan-to-fill opt-in onto the served column so
			// a consumer deriving the modal from table metadata still sees it.
			Scan: c.Scan,
			// VisibleWhen rides through onto the served column so a consumer that
			// derives the modal from table metadata still sees the predicate.
			VisibleWhen: toVisibleWhen(c.VisibleWhen),
			// Section rides through onto the served column so a consumer that
			// derives the modal from table metadata still sees the form grouping.
			Section: c.Section,
		}
		// Stage machine: when this column is the model's stage_field and the
		// model declares stages, derive a `status` display + the option list
		// (value/label/colour, ordered) straight from stages[] — the author does
		// not declare `options` separately. An explicit CellStyle/Options on the
		// column still wins (we only fill the gap).
		if c.Name == def.StageField && len(def.Stages) > 0 {
			if col.CellStyle == "" {
				col.CellStyle = "status"
			}
			if len(col.Options) == 0 {
				col.Options = stageOptions(def.Stages)
				col.UseOptions = len(col.Options) > 0
			}
		}
		// Auto-detect a prettier cell renderer for columns that ship no
		// explicit Display/CellStyle, inferred from the column name/type. An
		// author-declared CellStyle always wins.
		if col.CellStyle == "" {
			col.CellStyle = inferCellStyle(c.Name, c.Type)
		}
		out = append(out, col)
	}
	return out
}

// stageOptions projects a model's declarative stages onto the served
// modelbase.OptionDef list the SDK renders for the stage_field column: each
// stage becomes a {value:key, label, color} option, ordered by Stage.Order so a
// status select / board renders the lifecycle left-to-right without a separate
// `options` declaration. Returns nil for an empty list.
func stageOptions(stages []manifest.StageDef) []modelbase.OptionDef {
	if len(stages) == 0 {
		return nil
	}
	ordered := make([]manifest.StageDef, len(stages))
	copy(ordered, stages)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Order < ordered[j].Order
	})
	out := make([]modelbase.OptionDef, 0, len(ordered))
	for _, st := range ordered {
		label := st.Label
		if label == "" {
			label = st.Key
		}
		out = append(out, modelbase.OptionDef{
			Value: st.Key,
			Label: label,
			Color: st.Color,
		})
	}
	return out
}

// inferCellStyle guesses a rich cell renderer from a column's name and storage
// type when the manifest declares none. Heuristics are case-insensitive and key
// off common naming conventions (suffixes like `_url`/`_by`/`_at`, or whole
// names like `email`/`status`/`color`). It returns "" when nothing better than
// the plain text fallback applies, so the caller keeps CellStyle empty and the
// SDK renders the default text cell.
func inferCellStyle(name, storageType string) string {
	n := strings.ToLower(name)
	ui := ColumnUIType(storageType)

	// Timestamps/dates: by name suffix or storage type.
	if strings.HasSuffix(n, "_at") || ui == "datetime" {
		return "date"
	}
	switch n {
	case "url", "website", "link":
		return "url"
	case "email", "correo":
		return "email"
	case "phone", "telefono", "tel":
		return "phone"
	case "status", "state", "estado":
		return "status"
	case "color":
		return "color"
	case "image", "photo", "logo":
		return "image"
	}
	if strings.HasSuffix(n, "_url") || strings.HasSuffix(n, "_link") {
		return "url"
	}
	if strings.HasSuffix(n, "_image") || strings.HasSuffix(n, "_photo") ||
		strings.HasSuffix(n, "avatar") {
		return "image"
	}
	// Creator/owner audit columns → render the actor (avatar + name).
	switch n {
	case "created_by", "updated_by", "owner", "author":
		return "creator"
	}
	if strings.HasSuffix(n, "_by") {
		return "creator"
	}
	// Money columns: name match AND a numeric storage type.
	if ui == "number" && moneyColumn(n) {
		return "currency"
	}
	// Booleans get a dedicated renderer.
	if ui == "boolean" {
		return "boolean"
	}
	return ""
}

// moneyColumn reports whether a (lowercased) column name reads as a monetary
// amount. Pure name heuristic; the caller additionally gates on a numeric type.
func moneyColumn(n string) bool {
	switch n {
	case "price", "amount", "total", "cost", "subtotal", "balance", "paid":
		return true
	}
	for _, suf := range []string{"_price", "_amount", "_total", "_cost", "_subtotal", "_balance", "_paid"} {
		if strings.HasSuffix(n, suf) {
			return true
		}
	}
	return false
}

// toOptionDefs projects a declared column's static choice list
// (manifest.Option, carried from the v3 Column.Options) onto the served
// modelbase.OptionDef list the SDK reads. The optional icon/color/image visuals
// ride across verbatim so a status/brand select renders richly. Returns nil for
// an empty list so the omitempty json tag drops the field for plain columns.
func toOptionDefs(in []manifest.Option) []modelbase.OptionDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]modelbase.OptionDef, 0, len(in))
	for _, o := range in {
		out = append(out, modelbase.OptionDef{
			Value: o.Value,
			Label: o.Label,
			Icon:  o.Icon,
			Color: o.Color,
			Image: o.Image,
			When:  mapModelbaseOptionCondition(o.When),
		})
	}
	return out
}

// mapModelbaseOptionCondition projects the legacy static-option cascade guard
// (manifest.OptionCondition) onto the served modelbase.OptionCondition. Nil
// stays nil so plain enum options without a `when` block are unaffected.
func mapModelbaseOptionCondition(w *manifest.OptionCondition) *modelbase.OptionCondition {
	if w == nil {
		return nil
	}
	return &modelbase.OptionCondition{
		Field: w.Field,
		In:    w.In,
		NotIn: w.NotIn,
	}
}

// toFieldOptionsConfig projects the legacy DYNAMIC options carrier
// (manifest.DynamicOptionsDef, from the v3 `options` object form) onto the
// served modelbase.FieldOptionsConfig so the host's OptionsConfigResolver reads
// {source,filter_by,value,label_ref,description} straight off the metadata. The
// served Type is "dynamic" so the kernel's Service.Options queries Source.
// Returns nil for an absent declaration so the omitempty json tag drops it.
func toFieldOptionsConfig(in *manifest.DynamicOptionsDef) *modelbase.FieldOptionsConfig {
	if in == nil {
		return nil
	}
	return &modelbase.FieldOptionsConfig{
		Type:        "dynamic",
		Source:      in.Source,
		FilterBy:    in.FilterBy,
		Value:       in.Value,
		Label:       in.Label,
		LabelRef:    in.LabelRef,
		Description: in.Description,
	}
}

// toVisibleWhen projects the legacy conditional-visibility carrier onto the
// served modelbase.VisibleWhen the SDK reads. Nil stays nil (always visible).
func toVisibleWhen(in *manifest.VisibleWhenDef) *modelbase.VisibleWhen {
	if in == nil {
		return nil
	}
	return &modelbase.VisibleWhen{
		Field:  in.Field,
		Equals: in.Equals,
		In:     in.In,
	}
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
		} else if c.Ref != "" {
			// A column declaring a FK target renders as a searchable relation
			// picker in the native form even when the author left Widget empty.
			ftype = "dynamic_select"
		} else if c.OptionsConfig != nil {
			// A column declaring a DYNAMIC options source (the object form, a
			// dependent picker) renders as a searchable dynamic_select whose
			// candidates stream from /api/options scoped by the cascade field.
			ftype = "dynamic_select"
		} else if len(c.Options) > 0 {
			// A column declaring a static choice list renders as a plain select.
			ftype = "select"
		} else if c.OptionsSource != "" {
			// A column declaring a dynamic options provider also renders as a
			// select: the HOST materialises the provider's localized options
			// onto the served field, so the input plane is the same as a
			// static choice list — only the option source differs.
			ftype = "select"
		} else if ftype == "text" && imageColumn(c.Name) {
			ftype = "image"
		} else if ftype == "text" && longTextColumn(c.Name) {
			ftype = "textarea"
		}
		out = append(out, modelbase.FieldDef{
			Key:      c.Name,
			Label:    columnLabel(c),
			Type:     ftype,
			Widget:   c.Widget,
			Required: c.Required,
			// Nullable is the EXPLICIT inverse of Required (manifest Required is
			// itself derived from v3 Column.NotNull): a non-required column accepts
			// NULL, so the SDK submits `null` for an empty value instead of
			// inferring it from an empty "" / nil-UUID. An optional Ref picker is
			// the motivating case — it is nullable here so the front stops
			// reimplementing normalize-submit.ts.
			Nullable: !c.Required,
			// Ref (dynamic_select target) and Options (static select) project
			// onto the served form field so the SDK renders the picker/select
			// in the native create/edit modal without a custom action.
			Ref:     c.Ref,
			Options: toOptionDefs(c.Options),
			// OptionsSource rides through so the host materialises the
			// provider's options onto the served form field.
			OptionsSource: c.OptionsSource,
			// DependsOn + OptionsConfig forward the dependent-picker declaration
			// so the SDK scopes + re-fetches and the host resolves the options.
			DependsOn:     c.DependsOn,
			OptionsConfig: toFieldOptionsConfig(c.OptionsConfig),
			// Scan carries the camera scan-to-fill opt-in onto the served form
			// field (e.g. a product SKU input gets a barcode scan button).
			Scan: c.Scan,
			// Readonly marks a system-generated field: the SDK hides it from the
			// create form and disables it in edit (the value is written
			// server-side, e.g. by an addon's outbound sync).
			Readonly: c.Readonly,
			// VisibleWhen projects the conditional-visibility predicate onto the
			// served form field so the SDK renders it only when the referenced
			// sibling field's value matches. A hidden field never gates submit.
			VisibleWhen: toVisibleWhen(c.VisibleWhen),
			// Section places the field inside a form_layout section/step (the SDK
			// groups fields by Section against the model's DeriveFormLayout). Empty
			// = the implicit "General" block.
			Section: c.Section,
		})
	}
	return out
}

// DeriveFormLayout projects a model definition's create/edit form grouping onto
// the served modelbase.FormLayout the SDK reads to render collapsible sections
// (mode "sections") or a step wizard (mode "steps"). Nil stays nil (a flat
// form). Fields bind to a section/step via FieldDef.Section == FormSection.Key.
// Pure UI metadata; the DDL/write planes ignore it.
func DeriveFormLayout(def manifest.ModelDefinition) *modelbase.FormLayout {
	if def.FormLayout == nil {
		return nil
	}
	out := &modelbase.FormLayout{Mode: def.FormLayout.Mode}
	for _, s := range def.FormLayout.Sections {
		out.Sections = append(out.Sections, modelbase.FormSection{
			Key:         s.Key,
			Title:       s.Title,
			Description: s.Description,
			Collapsed:   s.Collapsed,
			VisibleWhen: toVisibleWhen(s.VisibleWhen),
		})
	}
	return out
}

// columnLabel returns the label the derivation should emit for a column: the
// author-DECLARED label when present (a literal header or an i18n key the
// host's localized transformer resolves at serve time — see DeriveTableColumns'
// two-stage contract), else the humanized column name. Keeping this in one
// place means table and form derivation stay in lock-step on label resolution.
func columnLabel(c manifest.ColumnDef) string {
	if c.Label != "" {
		return c.Label
	}
	return humanizeColumnName(c.Name)
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
