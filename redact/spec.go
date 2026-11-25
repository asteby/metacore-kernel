package redact

import "strings"

// RuleSpec is the language-neutral description of a built-in rule, exported
// as testdata/rules.json so ports (PHP, TypeScript) can load the same list
// instead of retyping it. Patterns are RE2; Flags holds the leading inline
// flags that were lifted out of Pattern ("i" case-insensitive, "s" dot
// matches newline).
type RuleSpec struct {
	Name         string `json:"name"`
	Pattern      string `json:"pattern"`
	Flags        string `json:"flags,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
	Suffix       string `json:"suffix,omitempty"`
	Check        string `json:"check,omitempty"`
	Extra        bool   `json:"extra,omitempty"`
	Optional     bool   `json:"optional,omitempty"`
	LegacyMarker string `json:"legacy_marker,omitempty"`
	Marker       string `json:"marker"`
}

// Spec returns the built-in rules in application order.
func Spec() []RuleSpec {
	out := make([]RuleSpec, 0, len(builtin))
	for _, r := range builtin {
		p, flags := r.Pattern.String(), ""
		for strings.HasPrefix(p, "(?") && len(p) > 4 && p[3] == ')' {
			flags += string(p[2])
			p = p[4:]
		}
		out = append(out, RuleSpec{
			Name: r.Name, Pattern: p, Flags: flags, Prefix: r.Prefix, Suffix: r.Suffix,
			Check: r.CheckName, Extra: r.Extra, Optional: r.Optional,
			LegacyMarker: r.LegacyMarker, Marker: marker(r.Name),
		})
	}
	return out
}
