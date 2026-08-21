package validate

import (
	"strconv"
	"strings"
)

// ParseRuleString understands both Laravel pipes (`required|min:2|email`) and
// the go-playground / compiled-model form ops already authors
// (`required,min=2,max=100`). A single unknown token becomes Custom (a slug
// like `rfc.tax_id` or `$org.tax_id_validator`). Empty → zero Spec.
func ParseRuleString(s string) Spec {
	s = strings.TrimSpace(s)
	if s == "" {
		return Spec{}
	}
	parts := splitRules(s)
	var spec Spec
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name, param := splitNameParam(p)
		switch strings.ToLower(name) {
		case "required":
			spec.Required = true
		case "nullable", "sometimes", "present":
			// Presence modifiers: required is the only one Check enforces.
			// nullable/sometimes mean "skip the rest when empty" which Check
			// already does for non-required specs.
		case "min":
			if n, err := strconv.ParseFloat(param, 64); err == nil {
				spec.Min = &n
			}
		case "max":
			if n, err := strconv.ParseFloat(param, 64); err == nil {
				spec.Max = &n
			}
		case "regex":
			spec.Regex = trimLaravelRegex(param)
		case "email", "uuid", "url", "numeric", "integer", "int":
			if spec.Custom == "" {
				spec.Custom = strings.ToLower(name)
				if spec.Custom == "int" {
					spec.Custom = CodeInteger
				}
			}
		case "in":
			for _, v := range strings.Split(param, ",") {
				v = strings.TrimSpace(v)
				if v != "" {
					spec.Options = append(spec.Options, v)
				}
			}
		default:
			if spec.Custom == "" {
				spec.Custom = p
			}
		}
	}
	return spec
}

func splitRules(s string) []string {
	if strings.Contains(s, "|") {
		return strings.Split(s, "|")
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name, _ := splitNameParam(p)
		if !isRuleName(strings.ToLower(name)) && len(out) > 0 {
			prev, _ := splitNameParam(out[len(out)-1])
			if strings.ToLower(prev) == "in" {
				out[len(out)-1] = out[len(out)-1] + "," + p
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

func isRuleName(name string) bool {
	switch name {
	case "required", "nullable", "sometimes", "present",
		"min", "max", "regex", "email", "uuid", "url",
		"numeric", "integer", "int", "in":
		return true
	}
	return false
}

func splitNameParam(p string) (name, param string) {
	if i := strings.IndexByte(p, ':'); i >= 0 {
		return strings.TrimSpace(p[:i]), strings.TrimSpace(p[i+1:])
	}
	if i := strings.IndexByte(p, '='); i >= 0 {
		return strings.TrimSpace(p[:i]), strings.TrimSpace(p[i+1:])
	}
	return p, ""
}

// Laravel writes regex:/^[A-Z]+$/ (slashes optional). Strip a matching pair
// so regexp.Compile gets the raw pattern.
func trimLaravelRegex(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 && p[0] == '/' {
		if i := strings.LastIndexByte(p, '/'); i > 0 {
			return p[1:i]
		}
	}
	return p
}
