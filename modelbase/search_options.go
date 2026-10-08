package modelbase

// SearchConfig describes how a model answers text-search queries. Every app
// that exposes /api/search/:model consumes this shape; kernel/dynamic.Service
// uses it directly for its Search method, and apps alias it into their own
// `models` package alongside TableMetadata / ModalMetadata so compiled models
// can return it from DefineSearch() without a conversion layer.
type SearchConfig struct {
	SearchIn    []string `json:"searchIn"`
	Value       string   `json:"value"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Image       string   `json:"image"`
	Icon        string   `json:"icon"`
	Preload     []string `json:"preload"`
	OrderBy     string   `json:"orderBy"`
	OrderDir    string   `json:"orderDir"`

	// BaseWhere is a fixed SQL condition ANDed into every search (e.g. the
	// anti-orphan guard "pharmacy_reviews.pharmacy_id IN (SELECT id FROM
	// pharmacies WHERE deleted_at IS NULL)"). Use "?" placeholders and put the
	// values in BaseArgs. SECURITY: it is raw SQL and MUST come from the
	// compiled model definition only; never build it from client input.
	// Empty = no extra restriction.
	BaseWhere string `json:"baseWhere,omitempty"`
	// BaseArgs are the placeholder values bound to BaseWhere, in order.
	BaseArgs []any `json:"baseArgs,omitempty"`
	// Joins are extra JOIN clauses applied to every search (e.g. "JOIN users
	// ON users.id = doctors.user_id"). Same SECURITY rule as BaseWhere:
	// compiled-model origin only, raw SQL, never client input.
	Joins []string `json:"joins,omitempty"`
	// AllowFilters is the allow-list of columns of the root table that the
	// client may filter by equality through query params (?brand=Bayer). Each
	// name must be a plain identifier; the client value is always bound as a
	// placeholder. Params for columns outside the list are IGNORED (never an
	// error, never reach SQL). Empty = no client filters.
	AllowFilters []string `json:"allowFilters,omitempty"`
	// ExtraFields names additional scalar columns of the row returned on every
	// hit as sibling keys of id/value/label. Only plain identifiers with
	// string/number/bool (or Stringer) values are returned; non-scalar values
	// (relations) and names reserved by the hit shape are skipped.
	ExtraFields []string `json:"extraFields,omitempty"`
}

// OptionsConfig declares per-field option sources for a model. Consumed by
// DynamicSelect-style components on the frontend and served by
// kernel/dynamic.Service.Options.
type OptionsConfig struct {
	Fields map[string]FieldOptionsConfig `json:"fields"`
}

// FieldOptionsConfig is the per-field description used by Service.Options to
// either return a static list or query a related model.
type FieldOptionsConfig struct {
	// Type is either "static" (use Options verbatim) or "dynamic" (query Source).
	Type string `json:"type"`

	// Dynamic-source fields — used when Type == "dynamic".
	Source      string `json:"source"`
	FilterBy    string `json:"filter_by"`
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Image       string `json:"image"`
	OrderBy     string `json:"orderBy"`
	OrderDir    string `json:"orderDir"`

	// LabelRef names a RELATED model (key or table) whose row, looked up by the
	// option's Value (a foreign id), supplies the human-readable label. It is the
	// relational twin of Label: Label reads a column ON the Source row, LabelRef
	// resolves the label from ANOTHER model by id. Use it when Source is a join /
	// scope table (e.g. a stock ledger) whose "name" column is itself a foreign
	// id — the picker shows the related record's name instead of the raw id.
	//
	// When set, Service.Options batch-resolves the labels from LabelRef in a
	// SINGLE query (no N+1) and fills Option.Label/Name for every option whose
	// projected label is blank or equal to its value. The target label column is
	// derived generically from LabelRef's model metadata (the same name-like
	// column preference EnableSelfOptions uses) — no model name is hardcoded.
	// Empty = no enrichment (label stays whatever Label projected). Optional.
	LabelRef string `json:"label_ref"`

	// ExtraColumns names additional scalar columns of the Source row that
	// Service.Options returns on EVERY option, as sibling keys of id/value/
	// label (e.g. ["status"] adds `"status":"cancelada"`). They are what a
	// field's option_filter tests client-side. Only plain identifiers are
	// honoured (unsafe names are ignored), a name that collides with a
	// reserved option key (id, value, label, name, description, image, color,
	// icon) is ignored, and only string/number/bool values are returned.
	// Empty = options carry no extra columns (retro-compatible). Optional.
	ExtraColumns []string `json:"extra_columns,omitempty"`

	// Static options — used when Type == "static".
	Options []StaticOption `json:"options"`
}

// StaticOption is an inline option.
type StaticOption struct {
	Value any    `json:"value"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	Color string `json:"color,omitempty"`
}
