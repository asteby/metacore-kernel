package tool

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
)

// ValidationError flags a single param that failed normalization, type
// coercion, or regex validation. The map return of Validate aggregates them
// per param name.
type ValidationError struct {
	Param  string
	Reason string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("tool.Validate: %s: %s", e.Param, e.Reason)
}

// Validate normalizes and validates raw params against a ToolInputParam list.
// Returns the cleaned params plus a non-nil slice of ValidationError when
// any input fails. Caller decides whether partial success is acceptable.
//
// Behavior summary per field:
//
//   - Required:      missing/empty → error.
//   - DefaultValue:  applied when missing (strings only; use ToolDef.Settings
//     for typed defaults).
//   - Normalize:     uppercase | lowercase | trim | alphanumeric | order_id.
//   - Type:          string | number | date | boolean | email | phone.
//   - Validation:    RE2 regex applied to the string form.
//   - FormatPattern: currently informational (applied by downstream tool).
func Validate(schema []manifest.ToolInputParam, raw map[string]any) (map[string]any, []ValidationError) {
	out := make(map[string]any, len(schema))
	var errs []ValidationError

	for _, p := range schema {
		v, present := raw[p.Name]
		if !present || isEmpty(v) {
			if p.DefaultValue != "" {
				v = p.DefaultValue
				present = true
			} else if p.Required {
				errs = append(errs, ValidationError{p.Name, "required"})
				continue
			} else {
				continue
			}
		}

		s := toString(v)
		s = applyNormalize(p.Normalize, s)

		if err := checkType(p.Type, s); err != nil {
			errs = append(errs, ValidationError{p.Name, err.Error()})
			continue
		}

		if p.Validation != "" {
			re, err := regexp.Compile(p.Validation)
			if err != nil {
				errs = append(errs, ValidationError{p.Name, "invalid validation regex: " + err.Error()})
				continue
			}
			if !re.MatchString(s) {
				errs = append(errs, ValidationError{p.Name, "does not match pattern"})
				continue
			}
		}

		out[p.Name] = coerceType(p.Type, s)
	}
	return out, errs
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(v)
	}
}

// normalizeRules maps the declared Normalize verb to its transformation. Keeping
// these in a map makes the set explicit and easy to extend.
var normalizeRules = map[string]func(string) string{
	"uppercase":    strings.ToUpper,
	"lowercase":    strings.ToLower,
	"trim":         strings.TrimSpace,
	"alphanumeric": keepAlphanumeric,
	"order_id":     orderIDNormalize,
}

func applyNormalize(rule, s string) string {
	s = strings.TrimSpace(s)
	if fn, ok := normalizeRules[rule]; ok {
		return fn(s)
	}
	return s
}

func keepAlphanumeric(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// orderIDNormalize uppercases + keeps dashes so an LLM-transcribed
// "abc-1234-xyz" maps to the canonical on-the-wire form.
func orderIDNormalize(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, " ", ""))
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
var phoneRe = regexp.MustCompile(`^\+?[0-9\s\-()]{6,}$`)

func checkType(t, s string) error {
	switch t {
	case "", "string":
		return nil
	case "number":
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return fmt.Errorf("expected number, got %q", s)
		}
	case "boolean":
		if _, err := strconv.ParseBool(s); err != nil {
			return fmt.Errorf("expected boolean, got %q", s)
		}
	case "date":
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			if _, err2 := time.Parse("2006-01-02", s); err2 != nil {
				return fmt.Errorf("expected RFC3339 or YYYY-MM-DD date, got %q", s)
			}
		}
	case "email":
		if !emailRe.MatchString(s) {
			return fmt.Errorf("invalid email %q", s)
		}
	case "phone":
		if !phoneRe.MatchString(s) {
			return fmt.Errorf("invalid phone %q", s)
		}
	}
	return nil
}

// coerceType returns the runtime-typed value for a string input. Anything that
// failed checkType returns the original string so callers can still log it.
func coerceType(t, s string) any {
	switch t {
	case "number":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			if f == float64(int64(f)) {
				return int64(f)
			}
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	}
	return s
}
