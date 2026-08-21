package validate

// Issue is one locale-agnostic failure on a single field. Code is a stable
// token from this package; Params carry interpolatable detail (min, max,
// kind, pattern, allowed, expected, attribute).
type Issue struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// Spec is the declarative constraint set a Check call enforces. Every field
// is optional and combines additively — same contract as manifest.ValidationRule
// plus Required (which lives on the column/field, not inside the rule object).
type Spec struct {
	Required bool
	Type     string
	Regex    string
	Min      *float64
	Max      *float64
	Custom   string
	// Options, when non-empty, is the closed enum the value must be in
	// (invalid_option). Distinct from Custom.
	Options []string
}

func (s Spec) empty() bool {
	return !s.Required && s.Regex == "" && s.Min == nil && s.Max == nil && s.Custom == "" && len(s.Options) == 0
}

func issue(code string, params map[string]any) Issue {
	if len(params) == 0 {
		return Issue{Code: code}
	}
	return Issue{Code: code, Params: params}
}
