package dynamic

// OptionsConfig describes how a model exposes option lists for its fields.
// Apps return this from OptionsConfigResolver — e.g. from a HasMetadata
// interface method on the model or from an addon registry.
type OptionsConfig struct {
	Fields map[string]FieldOptionsConfig `json:"fields"`
}

// FieldOptionsConfig is the per-field description used by Service.Options to
// either return a static list or query a related model.
type FieldOptionsConfig struct {
	// Type is either "static" (use Options verbatim) or "dynamic" (query Source).
	Type string `json:"type"`

	// Static options — used when Type == "static".
	Options []StaticOption `json:"options,omitempty"`

	// Dynamic-source fields — used when Type == "dynamic".
	Source      string `json:"source,omitempty"`
	FilterBy    string `json:"filter_by,omitempty"`
	Value       string `json:"value,omitempty"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	OrderBy     string `json:"orderBy,omitempty"`
	OrderDir    string `json:"orderDir,omitempty"`
}

// StaticOption is an inline option.
type StaticOption struct {
	Value any    `json:"value"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	Color string `json:"color,omitempty"`
}

// SearchConfig describes how a model answers text-search queries, including
// the columns to match against and how to project each hit into the common
// {id,value,label,description,image,icon} envelope consumed by Dynamic*
// frontend components.
type SearchConfig struct {
	SearchIn    []string `json:"searchIn"`
	Value       string   `json:"value"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Image       string   `json:"image,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Preload     []string `json:"preload,omitempty"`
	OrderBy     string   `json:"orderBy,omitempty"`
	OrderDir    string   `json:"orderDir,omitempty"`
}

// Option is the projected envelope returned by Options and Search. The dual
// id/value and label/name fields are preserved for legacy frontend parity —
// existing Dynamic* React components read either key.
type Option struct {
	ID          any `json:"id"`
	Value       any `json:"value"`
	Label       any `json:"label"`
	Name        any `json:"name"`
	Description any `json:"description,omitempty"`
	Image       any `json:"image,omitempty"`
	Color       any `json:"color,omitempty"`
	Icon        any `json:"icon,omitempty"`
}
