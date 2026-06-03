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
	// Validation declares server-side input constraints that the SDK can
	// also pre-flight in the form layer. Strings prefixed with `$org.`
	// (e.g. `$org.tax_id_validator`) are resolved at runtime against the
	// current organization's config — keeping fiscal/regional rules out of
	// the kernel and out of the SDK.
	Validation *ValidationRule `json:"validation,omitempty"`
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
	Key            string      `json:"key"`
	Label          string      `json:"label"`
	Type           string      `json:"type"` // text, textarea, select, search, number, date, email, url, boolean, image, dynamic_select, array
	Required       bool        `json:"required,omitempty"`
	Validation     string      `json:"validation,omitempty"`
	Options        []OptionDef `json:"options,omitempty"`
	DefaultValue   interface{} `json:"defaultValue,omitempty"`
	HideInView     bool        `json:"hideInView,omitempty"`
	SearchEndpoint string      `json:"searchEndpoint,omitempty"`
	Placeholder    string      `json:"placeholder,omitempty"`
	Ref            string      `json:"ref,omitempty"`

	// Widget overrides the renderer inferred from Type (e.g. "textarea",
	// "dynamic_select"). Optional — empty lets the SDK infer from Type.
	Widget string `json:"widget,omitempty"`

	// ItemFields declares the columns of a repeatable line-items group, set on a
	// field with Type "array" (e.g. the debit/credit lines of a journal entry).
	// Each entry is itself a FieldDef describing one cell widget; the field's
	// value is an array of objects keyed by these item-field keys. The SDK
	// (runtime-react dynamic-line-items) renders a row grid. Mirrors
	// manifest/v3 ActionField.item_fields so the rich modal survives the
	// v3 → host conversion (previously dropped, collapsing the grid to a single
	// text input).
	ItemFields []FieldDef `json:"item_fields,omitempty"`

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
	Type           string      `json:"type,omitempty"`      // custom, link
	LinkURL        string      `json:"linkUrl,omitempty"`   // URL pattern for type=link
	Placement      string      `json:"placement,omitempty"` // "row" (default), "table", or "create" — see manifest/v3.Action.Placement
	ModalWidth     string      `json:"modalWidth,omitempty"` // explicit modal width (CSS length / px); SDK reads action.modalWidth. Must match manifest.ActionDef.ModalWidth so the host→SDK JSON round-trip preserves it.
	Condition      interface{} `json:"condition,omitempty"`
	Confirm        bool        `json:"confirm,omitempty"`
	ConfirmMessage string      `json:"confirmMessage,omitempty"`
	Fields         []FieldDef  `json:"fields,omitempty"`
	RequiresState  []string    `json:"requiresState,omitempty"`
	IsCollection   bool        `json:"isCollection,omitempty"`
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
}

// KV is an alias retained for backwards compatibility with older call-sites
// that referred to the option pair as KV.
type KV = OptionDef
