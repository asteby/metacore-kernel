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
