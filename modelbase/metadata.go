package modelbase

// This file holds the public, framework-agnostic metadata shapes that the
// frontend DynamicTable / DynamicModal / DynamicSearch components consume via
// JSON. The JSON tags are load-bearing and MUST NOT drift from the frontend
// contract.
//
// App-specific concerns (e.g. branch-scoped filtering) are intentionally NOT
// represented here — apps layer those on via their own interfaces.
// TODO(apps): models needing branch scoping implement BranchScoper in their
// own package, not the kernel.

// TableMetadata describes a table view rendered by the frontend.
type TableMetadata struct {
	Title             string      `json:"title"`
	Columns           []ColumnDef `json:"columns"`
	SearchColumns     []string    `json:"searchColumns,omitempty"`
	Filters           []FilterDef `json:"filters,omitempty"`
	Actions           []ActionDef `json:"actions,omitempty"`
	EnableCRUDActions bool        `json:"enableCRUDActions,omitempty"`
	PerPageOptions    []int       `json:"perPageOptions,omitempty"`
	DefaultPerPage    int         `json:"defaultPerPage,omitempty"`
	SearchPlaceholder string      `json:"searchPlaceholder,omitempty"`

	// Relations are the inverse 1:N / N:M edges the frontend renders as
	// "related records" panels on a detail page (e.g. a Customer's vehicles,
	// addresses and attachments). Populated by the metadata service from the
	// model's HasRelations / manifest-declared relations — empty for flat
	// models, so the field is omitted from their served payload. The JSON tag
	// is load-bearing: it MUST match what the SDK's <DynamicRelations> reads.
	Relations []RelationMeta `json:"relations,omitempty"`

	// FormLayout declares declarative grouping for the model's create/edit form —
	// collapsible sections on one scroll (mode "sections") or a step wizard (mode
	// "steps"). Fields bind to a section/step via FieldDef.Section. Populated by
	// the metadata service from the manifest-declared form_layout; nil for models
	// that ship a flat form. Pure UI metadata; the DDL/write planes ignore it.
	FormLayout *FormLayout `json:"form_layout,omitempty"`
}

// FormLayout is the served grouping spec for a model's create/edit form. Mode
// "sections" renders Sections as titled, collapsible blocks on one scroll; mode
// "steps" renders them as a wizard (the SDK validates step-by-step). A field
// binds to a section/step via FieldDef.Section == FormSection.Key; unbound
// fields fall into an implicit "General" block. It mirrors manifest/v3 FormLayout
// so the block round-trips byte-for-byte through the host's served metadata.
type FormLayout struct {
	// Mode is "sections" (collapsible blocks, default) or "steps" (wizard).
	Mode string `json:"mode"`
	// Sections is the ordered list of blocks/steps.
	Sections []FormSection `json:"sections"`
}

// FormSection is one block of the create/edit form, reused for both a
// collapsible section and a wizard step. Title/Description may already be
// localized by the host's transformer or carry an i18n key the SDK resolves.
type FormSection struct {
	Key         string `json:"key"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Collapsed   bool   `json:"collapsed,omitempty"`
	// VisibleWhen hides the whole section when the predicate is false against the
	// live form values (same {field, equals?, in?} shape as a field's
	// visible_when). Nil = always visible. The SDK does the show/hide.
	VisibleWhen *VisibleWhen `json:"visible_when,omitempty"`
}

// RelationMeta is one inverse relation projected onto served TableMetadata so
// the SDK can render a related-records panel without per-app wiring. It mirrors
// the relation vocabulary the addon declares in its manifest (and that compiled
// models expose via HasRelations); the JSON tags match manifest.RelationDef /
// manifest/v3 ModelRelation so the value round-trips byte-for-byte through the
// host's metadata payload.
//
// Scope carries a static equality filter applied to the child query, which is
// what makes polymorphic children addressable (e.g. {"owner_model":"Customer"}
// on a shared attachments table). Empty Scope = no extra filter.
type RelationMeta struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"` // "one_to_many" | "many_to_many"
	Through    string            `json:"through"`
	ForeignKey string            `json:"foreign_key"`
	Scope      map[string]string `json:"scope,omitempty"`
	Label      string            `json:"label,omitempty"`
	// Readonly declares that the relation panel must not allow create/edit/delete
	// of child rows (e.g. an append-only ledger like an inventory kardex).
	// Projected from manifest/v3 ModelRelation.readonly. Pure UI.
	Readonly bool `json:"readonly,omitempty"`
}

// ColumnDef describes a single column in a TableMetadata.
type ColumnDef struct {
	Key            string                 `json:"key"`
	Label          string                 `json:"label"`
	Type           string                 `json:"type"`
	Sortable       bool                   `json:"sortable,omitempty"`
	Filterable     bool                   `json:"filterable,omitempty"`
	Options        []OptionDef            `json:"options,omitempty"`
	UseOptions     bool                   `json:"useOptions,omitempty"`
	CellStyle      string                 `json:"cellStyle,omitempty"`
	StyleConfig    map[string]interface{} `json:"styleConfig,omitempty"`
	Tooltip        string                 `json:"tooltip,omitempty"`
	Description    string                 `json:"description,omitempty"`
	BasePath       string                 `json:"basePath,omitempty"`
	DisplayField   string                 `json:"displayField,omitempty"`
	IconField      string                 `json:"iconField,omitempty"`
	RelationPath   string                 `json:"relationPath,omitempty"`
	SearchEndpoint string                 `json:"searchEndpoint,omitempty"`
	Hidden         bool                   `json:"hidden,omitempty"`
	// Readonly marks a SYSTEM-GENERATED column (see manifest/v3 Column.readonly):
	// the host projects it so form derivation excludes it from create and marks
	// it read-only in edit. The column still renders in tables/detail. Pure UI.
	Readonly bool `json:"readonly,omitempty"`
	// Ref is the foreign-key target model the column points at (e.g.
	// "customers", "addon_tickets.comments"). When populated, the SDK
	// resolves the column's options against `/api/options/:Ref?field=id`
	// instead of falling back to a hand-wired SearchEndpoint or hardcoded
	// Options. Authors set Ref directly on compiled models; for addons it
	// is auto-derived by the metadata service from
	// `manifest.ModelDefinition.Relations` so a column named `customer_id`
	// targeting a belongs-to relation reports Ref="customers" without any
	// per-column declaration.
	Ref string `json:"ref,omitempty"`
	// OptionsSource names a DYNAMIC options provider (e.g.
	// "registered_models", "installed_addons") the HOST resolves when serving
	// this metadata: it materialises the provider's localized value/label list
	// onto Options (and typically sets UseOptions) so the SDK renders a select
	// / multi-select filter without the manifest hardcoding choices. The
	// kernel only carries the key (mirrors manifest/v3 Column.options_source —
	// json tag matches the v3 contract so it round-trips); providers are
	// host-registered, and an unknown key simply leaves Options empty.
	OptionsSource string `json:"options_source,omitempty"`
	// DependsOn names a SIBLING field whose current value supplies the cascade
	// `filter_value` of this column's dependent picker. Mirrors manifest/v3
	// Column.depends_on (json `depends_on`) so the SDK scopes + re-fetches the
	// dynamic_select options when the depended-on field changes. Empty = no
	// cascade (lists everything). Pure UI metadata; the DDL plane ignores it.
	DependsOn string `json:"depends_on,omitempty"`
	// OptionsConfig carries the DYNAMIC options DECLARATION (source / filter_by /
	// value / label_ref / description) for a dependent picker — the object form
	// of the v3 `options` block. The host reads it off the served metadata to
	// build the OptionsConfigResolver entry that the kernel's Service.Options
	// uses (scope by filter_by, project description, resolve label from
	// label_ref). It is distinct from the STATIC Options list above: a column
	// declares EITHER a static enum OR a dynamic source, never both. Nil = no
	// dynamic source. Pure UI/query metadata; the DDL plane ignores it.
	OptionsConfig *FieldOptionsConfig `json:"optionsConfig,omitempty"`
	// VisibleWhen declares CONDITIONAL VISIBILITY for the column when it is
	// projected into the create/edit modal (form derivation copies it onto the
	// served FieldDef). Nil = always visible. Pure UI metadata; the DDL plane
	// ignores it. See VisibleWhen.
	VisibleWhen *VisibleWhen `json:"visible_when,omitempty"`
	// Section assigns the column, when projected into the create/edit modal, to a
	// form_layout section/step by its key (form derivation copies it onto the
	// served FieldDef). Empty = the implicit "General" block. Pure UI metadata;
	// the DDL plane ignores it.
	Section string `json:"section,omitempty"`
	// Validation declares server-side input constraints that the SDK can
	// also pre-flight in the form layer. Strings prefixed with `$org.`
	// (e.g. `$org.tax_id_validator`) are resolved at runtime against the
	// current organization's config — keeping fiscal/regional rules out of
	// the kernel and out of the SDK.
	Validation *ValidationRule `json:"validation,omitempty"`
}

// VisibleWhen is a single-sibling conditional-visibility predicate the SDK
// evaluates against the current create/edit form values. Field names the OTHER
// form field whose value drives visibility; the owning field is shown when that
// value satisfies Equals (exact string match) OR is a member of In (any-of).
// Exactly one of Equals / In is meaningful — when both are set In wins. Empty
// (no Field) is a no-op (always visible). It mirrors manifest/v3 VisibleWhen so
// the block round-trips byte-for-byte through the host's served metadata.
type VisibleWhen struct {
	// Field is the sibling field key whose value drives this field's visibility.
	Field string `json:"field"`
	// Equals shows the owning field when the sibling value == this string.
	Equals string `json:"equals,omitempty"`
	// In shows the owning field when the sibling value is one of these strings.
	In []string `json:"in,omitempty"`
}

// ValidationRule mirrors `manifest.ValidationRule` but lives on the metadata
// payload exposed to the frontend. Apps can populate it directly on compiled
// models (HasMetadata) and the metadata service projects it from manifest
// authors automatically. The Custom field accepts either a literal validator
// identifier (e.g. "rfc.tax_id") OR a `$org.<key>` reference that the SDK
// resolves against the current org config — this is the contract that lets
// region-specific rules ride the same plumbing without leaking fiscal
// vocabulary into core.
type ValidationRule struct {
	Regex  string   `json:"regex,omitempty"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Custom string   `json:"custom,omitempty"`
}

// ModalMetadata describes a create/edit modal rendered by the frontend.
type ModalMetadata struct {
	Title         string            `json:"title"`
	CreateTitle   string            `json:"createTitle,omitempty"`
	EditTitle     string            `json:"editTitle,omitempty"`
	DeleteTitle   string            `json:"deleteTitle,omitempty"`
	Fields        []FieldDef        `json:"fields"`
	CustomActions []ActionDef       `json:"customActions,omitempty"`
	Messages      map[string]string `json:"messages,omitempty"`
}

// FieldDef describes a single form field inside a ModalMetadata.
//
// Validation accepts either a legacy literal pattern (e.g. "email") or a
// `$org.<key>` reference resolved at runtime against the org config — same
// contract as ColumnDef.Validation.Custom. Ref points at a foreign-key target
// model so the SDK can resolve the field's option list against the canonical
// `/api/options/:ref?field=id` endpoint.
type FieldDef struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // text, textarea, select, search, number, date, email, url, boolean, image, dynamic_select, array
	Required bool   `json:"required,omitempty"`

	// Nullable is the EXPLICIT nullability contract for the served form field:
	// true means the field accepts SQL NULL and the SDK must submit `null` (not
	// "" or the nil-UUID) when the user leaves it empty. Its canonical semantics
	// are the inverse of Required — a field is nullable exactly when it is not
	// NOT NULL — and in particular an OPTIONAL Ref (a relation/FK picker with
	// !Required) is nullable, which is the case the SDK previously had to INFER
	// with the `normalize-submit.ts` / `nil-uuid.ts` heuristics. Carrying the
	// flag explicitly on the metadata lets the SDK stop reimplementing that
	// inference: it reads `nullable` and emits `null` directly. `omitempty` keeps
	// the payload retro-compatible (a NOT NULL / required field simply omits it).
	Nullable bool `json:"nullable,omitempty"`

	Validation     string      `json:"validation,omitempty"`
	Options        []OptionDef `json:"options,omitempty"`
	DefaultValue   interface{} `json:"defaultValue,omitempty"`
	HideInView     bool        `json:"hideInView,omitempty"`
	SearchEndpoint string      `json:"searchEndpoint,omitempty"`
	Placeholder    string      `json:"placeholder,omitempty"`
	Ref            string      `json:"ref,omitempty"`

	// OptionsSource names a DYNAMIC options provider the HOST resolves when
	// serving this metadata, materialising the localized value/label list onto
	// Options so the form renders a select without hardcoded choices. Mirrors
	// manifest/v3 Column.options_source (json tag matches the v3 contract so
	// it round-trips). Providers are host-registered; unknown keys leave
	// Options empty.
	OptionsSource string `json:"options_source,omitempty"`

	// DependsOn names ANOTHER field in the same form whose current value supplies
	// this dependent picker's cascade `filter_value`. Mirrors manifest/v3
	// ActionField.depends_on / Column.depends_on (json `depends_on`) so the SDK
	// scopes + re-fetches the dynamic_select options when the depended-on field
	// changes. Empty = no cascade. Pure UI metadata.
	DependsOn string `json:"depends_on,omitempty"`

	// OptionsConfig carries the DYNAMIC options DECLARATION (source / filter_by /
	// value / label_ref / description) for a dependent picker — the object form
	// of the v3 `options` block. The host reads it off the served metadata to
	// build the OptionsConfigResolver entry Service.Options uses. Distinct from
	// the static Options list: a field declares EITHER a static enum OR a
	// dynamic source, never both. Nil = no dynamic source.
	OptionsConfig *FieldOptionsConfig `json:"optionsConfig,omitempty"`

	// Widget overrides the renderer inferred from Type (e.g. "textarea",
	// "dynamic_select"). Optional — empty lets the SDK infer from Type.
	Widget string `json:"widget,omitempty"`

	// Readonly marks a SYSTEM-GENERATED field the user must never hand-edit — a
	// value the addon/host populates server-side (e.g. an external id/number a
	// remote API returns). Mirrors manifest/v3 Column.readonly (json `readonly`).
	// The SDK hides a readonly field from the create form and disables it in the
	// edit form; the value still shows in read views. Pure UI metadata.
	Readonly bool `json:"readonly,omitempty"`

	// ItemFields declares the columns of a repeatable line-items group, set on a
	// field with Type "array" (e.g. the debit/credit lines of a journal entry).
	// Each entry is itself a FieldDef describing one cell widget; the field's
	// value is an array of objects keyed by these item-field keys. The SDK
	// (runtime-react dynamic-line-items) renders a row grid. Mirrors
	// manifest/v3 ActionField.item_fields so the rich modal survives the
	// v3 → host conversion (previously dropped, collapsing the grid to a single
	// text input).
	ItemFields []FieldDef `json:"item_fields,omitempty"`

	// LockRows, set on a Type "array" line-items field, declares that its rows
	// are FIXED: the SDK (dynamic-line-items) hides the add-row and delete-row
	// controls so the user can only edit existing rows' cells (e.g. a receive
	// form prefilled from the ordered lines). Mirrors manifest/v3
	// ActionField.lock_rows. Ignored on non-array fields.
	LockRows bool `json:"lock_rows,omitempty"`

	// Total flags an ItemFields column for summation in the line-items footer.
	Total bool `json:"total,omitempty"`

	// Balance declares a generic reconciliation constraint on a line-items
	// (Type "array") field: the summed Total of one column must equal another
	// (Σdebit == Σcredit). Drives the balanced/out-of-balance indicator and the
	// submit gate. Domain-agnostic.
	Balance *FieldBalanceRule `json:"balance,omitempty"`

	// Accept, MaxSize and StoragePath configure a Type "upload" field (a file
	// attachment input). Mirrors manifest/v3 ActionField so the upload
	// declaration survives the v3 → host conversion; the SDK reads them off the
	// served action metadata and the host's upload handler honours MaxSize /
	// StoragePath. Ignored on non-upload fields.
	Accept      string `json:"accept,omitempty"`
	MaxSize     int64  `json:"max_size,omitempty"`
	StoragePath string `json:"storage_path,omitempty"`

	// LabelImage/LabelIcon/LabelColor name a column on the REMOTE model a
	// dynamic_select / search field resolves against whose value the SDK
	// renders as a visual beside each option's label (a product thumbnail, a
	// brand icon, a status colour). Mirrors manifest/v3 ActionField so the
	// mapping survives the v3 → host conversion. Ignored on non-reference fields.
	LabelImage string `json:"label_image,omitempty"`
	LabelIcon  string `json:"label_icon,omitempty"`
	LabelColor string `json:"label_color,omitempty"`

	// VisibleWhen declares CONDITIONAL VISIBILITY for this field in the
	// create/edit modal: the SDK renders it only when the referenced sibling
	// field's current value matches the condition (see VisibleWhen). Nil = the
	// field is always visible (legacy behaviour). A hidden field's value never
	// gates submit. Pure UI metadata; the DDL and write planes ignore it.
	VisibleWhen *VisibleWhen `json:"visible_when,omitempty"`

	// Section places this field inside a form_layout section/step by its key (the
	// SDK groups fields by Section against the model's served FormLayout). Empty =
	// the implicit "General" block. Pure UI metadata; the DDL/write planes ignore it.
	Section string `json:"section,omitempty"`
}

// FieldBalanceRule is the host-facing mirror of manifest/v3 FieldBalanceRule.
// It reconciles two summed line-items columns. JSON tags match the v3 contract
// so it round-trips byte-for-byte through the host's action metadata.
type FieldBalanceRule struct {
	DebitColumn    string `json:"debit_column"`
	CreditColumn   string `json:"credit_column"`
	Message        string `json:"message,omitempty"`
	RequireNonzero *bool  `json:"require_nonzero,omitempty"`
}

// ActionDef is the UI metadata for a frontend action button. The backend
// handler that implements the action is bound separately per-app (handlers
// depend on the app's DB, which modelbase does not).
type ActionDef struct {
	Key            string      `json:"key"`
	Name           string      `json:"name"`
	Label          string      `json:"label"`
	Icon           string      `json:"icon,omitempty"`
	Class          string      `json:"class,omitempty"`
	Color          string      `json:"color,omitempty"`
	Type           string      `json:"type,omitempty"`       // custom, link
	LinkURL        string      `json:"linkUrl,omitempty"`    // URL pattern for type=link
	Placement      string      `json:"placement,omitempty"`  // "row" (default), "table", or "create" — see manifest/v3.Action.Placement
	ModalWidth     string      `json:"modalWidth,omitempty"` // explicit modal width (CSS length / px); SDK reads action.modalWidth. Must match manifest.ActionDef.ModalWidth so the host→SDK JSON round-trip preserves it.
	Condition      interface{} `json:"condition,omitempty"`
	Confirm        bool        `json:"confirm,omitempty"`
	ConfirmMessage string      `json:"confirmMessage,omitempty"`
	Fields         []FieldDef  `json:"fields,omitempty"`
	// Steps mirrors manifest.ActionDef.Steps (a declarative multi-step wizard);
	// JSON key matches the SDK's ActionMetadata.steps so the host→SDK round-trip
	// preserves it.
	Steps         []ActionStepDef `json:"steps,omitempty"`
	RequiresState []string        `json:"requiresState,omitempty"`
	IsCollection  bool            `json:"isCollection,omitempty"`
}

// ActionStepDef is one wizard page of a multi-step action form — the served
// twin of manifest.ActionStepDef.
type ActionStepDef struct {
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Fields      []FieldDef `json:"fields"`
}

// FilterDef describes a filter chip rendered above a TableMetadata.
type FilterDef struct {
	Key            string      `json:"key"`
	Label          string      `json:"label"`
	Type           string      `json:"type"`   // select, boolean, date_range, number_range, text
	Column         string      `json:"column"` // actual DB column for f_ param
	Default        interface{} `json:"default,omitempty"`
	Options        []OptionDef `json:"options,omitempty"`
	SearchEndpoint string      `json:"searchEndpoint,omitempty"`
}

// OptionDef represents a single option inside a select-like widget. Also known
// historically as KV / OptionPair — this is the canonical name going forward.
type OptionDef struct {
	Value interface{} `json:"value"`
	Label string      `json:"label"`
	Color string      `json:"color,omitempty"`
	Icon  string      `json:"icon,omitempty"`
	// Image is a URL (or bundle-relative path) to a small image / avatar /
	// logo the SDK renders beside the option label. Mirrors manifest/v3
	// FieldOption.image so a static option list can show a thumbnail. Optional.
	Image string `json:"image,omitempty"`
	// When is the OPTIONAL static cascade guard: it gates this option by the
	// current value of a sibling enum field (see OptionCondition). The SDK
	// filters the option list by the parent's value and hides the select when
	// nothing applies. Nil = the option always applies (retro-compatible).
	When *OptionCondition `json:"when,omitempty"`
}

// OptionCondition gates a single static OptionDef by the current value of a
// sibling enum field — the served form of the manifest/v3 OptionCondition. It
// is the static twin of the relational cascade (DependsOn + FieldOptionsConfig).
//
//	Field — the sibling field whose value is evaluated. Empty = fall back to the
//	        container ColumnDef/FieldDef.DependsOn.
//	In    — the option applies when the sibling's value ∈ In (string compare).
//	NotIn — the option applies when the sibling's value ∉ NotIn.
type OptionCondition struct {
	Field string   `json:"field,omitempty"`
	In    []string `json:"in,omitempty"`
	NotIn []string `json:"not_in,omitempty"`
}

// KV is an alias retained for backwards compatibility with older call-sites
// that referred to the option pair as KV.
type KV = OptionDef
