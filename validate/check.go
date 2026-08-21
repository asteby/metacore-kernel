package validate

import (
	"regexp"
	"strconv"
	"strings"
)

const nilUUIDString = "00000000-0000-0000-0000-000000000000"

var numericTypes = map[string]struct{}{
	"int":     {},
	"integer": {},
	"bigint":  {},
	"decimal": {},
	"numeric": {},
	"number":  {},
	"float":   {},
	"double":  {},
}

// Check evaluates spec against a raw input value and returns every issue in
// one pass (Laravel's ErrorBag, not fail-fast). Empty spec → nil. An empty
// value only produces `required`; every other rule is skipped so optional
// fields don't fire min/regex on blanks — same as Laravel `nullable`.
func Check(value any, spec Spec) []Issue {
	if spec.empty() {
		return nil
	}
	empty := isEmpty(value)
	if spec.Required && empty {
		return []Issue{issue(CodeRequired, nil)}
	}
	if empty {
		return nil
	}

	var out []Issue
	typ := strings.ToLower(strings.TrimSpace(spec.Type))

	if _, isNum := numericTypes[typ]; isNum && !isNumeric(value) {
		out = append(out, issue(CodeInvalidType, map[string]any{"expected": "number"}))
		return out
	}

	if len(spec.Options) > 0 {
		want := valueToString(value)
		ok := false
		for _, o := range spec.Options {
			if o == want {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, issue(CodeInvalidOption, map[string]any{"allowed": spec.Options}))
		}
	}

	if spec.Regex != "" && isStringy(typ) {
		if re, err := regexp.Compile(spec.Regex); err == nil {
			if !re.MatchString(valueToString(value)) {
				out = append(out, issue(CodeRegex, map[string]any{"pattern": spec.Regex}))
			}
		}
	}

	if spec.Min != nil || spec.Max != nil {
		out = append(out, checkBounds(value, typ, spec.Min, spec.Max)...)
	}

	if spec.Custom != "" {
		if fn := lookupCustom(spec.Custom, nil); fn != nil {
			if code, params := fn(value); code != "" {
				out = append(out, issue(code, params))
			}
		}
		// Unknown custom slug: skip (host/addon did not register it).
	}

	return out
}

// CheckWithResolver is Check plus a per-call custom-slug resolver. Hosts
// wire org-config / addon registries here; builtins still apply when the
// resolver returns nil.
func CheckWithResolver(value any, spec Spec, resolver Resolver) []Issue {
	if resolver == nil {
		return Check(value, spec)
	}
	// Re-run custom with the resolver by copying spec and applying custom
	// ourselves after the rest. Cheaper to call Check then replace custom.
	without := spec
	without.Custom = ""
	out := Check(value, without)
	if spec.Custom == "" || isEmpty(value) {
		return out
	}
	fn := lookupCustom(spec.Custom, resolver)
	if fn == nil {
		return out
	}
	if code, params := fn(value); code != "" {
		out = append(out, issue(code, params))
	}
	return out
}

func checkBounds(value any, typ string, min, max *float64) []Issue {
	_, isNum := numericTypes[typ]
	kind := KindLength
	n := lengthOf(value)
	if isNum {
		kind = KindValue
		n = numericValue(value)
	}
	var out []Issue
	if min != nil && n < *min {
		out = append(out, issue(CodeMin, map[string]any{"min": *min, "kind": kind}))
	}
	if max != nil && n > *max {
		out = append(out, issue(CodeMax, map[string]any{"max": *max, "kind": kind}))
	}
	return out
}

func isEmpty(raw any) bool {
	if raw == nil {
		return true
	}
	switch v := raw.(type) {
	case string:
		t := strings.TrimSpace(v)
		return t == "" || t == nilUUIDString
	case []any:
		return len(v) == 0
	case []map[string]any:
		return len(v) == 0
	}
	return false
}

func isStringy(typ string) bool {
	switch typ {
	case "", "string", "text", "varchar", "char", "email", "url", "uuid":
		return true
	}
	return false
}

func lengthOf(raw any) float64 {
	switch v := raw.(type) {
	case string:
		return float64(len([]rune(strings.TrimSpace(v))))
	case []any:
		return float64(len(v))
	case []map[string]any:
		return float64(len(v))
	default:
		return float64(len([]rune(valueToString(raw))))
	}
}

func numericValue(raw any) float64 {
	switch v := raw.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	case uint:
		return float64(v)
	case uint64:
		return float64(v)
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n
	default:
		n, _ := strconv.ParseFloat(valueToString(raw), 64)
		return n
	}
}
