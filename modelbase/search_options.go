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
