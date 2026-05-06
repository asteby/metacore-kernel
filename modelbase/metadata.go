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
	Type           string      `json:"type"` // text, textarea, select, search, number, date, email, url, boolean, image
	Required       bool        `json:"required,omitempty"`
	Validation     string      `json:"validation,omitempty"`
	Options        []OptionDef `json:"options,omitempty"`
	DefaultValue   interface{} `json:"defaultValue,omitempty"`
	HideInView     bool        `json:"hideInView,omitempty"`
	SearchEndpoint string      `json:"searchEndpoint,omitempty"`
	Placeholder    string      `json:"placeholder,omitempty"`
	Ref            string      `json:"ref,omitempty"`
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
	Type           string      `json:"type,omitempty"`    // custom, link
	LinkURL        string      `json:"linkUrl,omitempty"` // URL pattern for type=link
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
