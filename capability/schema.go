package capability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// FieldType is the JSON type of a contract field.
type FieldType string

const (
	// TypeAny accepts any JSON value (the type of an untyped field).
	TypeAny FieldType = ""
	// TypeString is a JSON string.
	TypeString FieldType = "string"
	// TypeDecimal is an exact decimal written as a JSON string ("199.90",
	// "-5", "0.0825"). Money is ALWAYS a decimal string next to a currency,
	// never a float.
	TypeDecimal FieldType = "decimal"
	// TypeCurrency is an ISO 4217 code ("MXN", "USD").
	TypeCurrency FieldType = "currency"
	// TypeCountry is an ISO 3166-1 alpha-2 code ("MX", "US").
	TypeCountry FieldType = "country"
	// TypeInteger is a JSON number without a fractional part.
	TypeInteger FieldType = "integer"
	// TypeNumber is any JSON number (scores, ratios — never money).
	TypeNumber FieldType = "number"
	// TypeBoolean is true or false.
	TypeBoolean FieldType = "boolean"
	// TypeObject is a JSON object; Field.Of, when set, is its shape.
	TypeObject FieldType = "object"
	// TypeArray is a JSON array; Field.Of, when set, is the shape of each
	// (object) element, otherwise Field.Elem is the element type.
	TypeArray FieldType = "array"
)

// Field is one field of a contract input or output.
type Field struct {
	Name     string
	Type     FieldType
	Required bool
	// Enum, when non-empty, lists the only accepted string values.
	Enum []string
	// Of is the shape of an object field, or of each element of an array
	// field. Nil = free-form.
	Of *Schema
	// Elem is the element type of an array of scalars (Of == nil).
	Elem FieldType
	// Pattern, when set on a string field, is a regular expression (RE2,
	// anchor it yourself) the value must match, e.g. `^[0-9]{5}$`.
	// Register rejects a contract whose pattern does not compile.
	Pattern string
	// Doc is a one-line description for the generated reference.
	Doc string
}

// Schema is the shape of a contract input or output object.
type Schema struct {
	Fields []Field
	// Open accepts fields the schema does not declare. A closed schema
	// (default) still accepts extension fields prefixed "x_" so a provider
	// can return its own extras without breaking the normalized shape.
	Open bool
}

// Field returns the named field.
func (s *Schema) Field(name string) (Field, bool) {
	if s == nil {
		return Field{}, false
	}
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Required lists the names of the required fields, in declaration order.
func (s *Schema) Required() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, f := range s.Fields {
		if f.Required {
			out = append(out, f.Name)
		}
	}
	return out
}

// Optional lists the names of the optional fields, in declaration order.
func (s *Schema) Optional() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, f := range s.Fields {
		if !f.Required {
			out = append(out, f.Name)
		}
	}
	return out
}

var (
	decimalRe  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
)

// ValidDecimal reports whether s is a decimal string ("12", "199.90", "-0.5").
func ValidDecimal(s string) bool { return decimalRe.MatchString(s) }

// ValidCountry reports whether s is an upper-case ISO 3166-1 alpha-2 code.
func ValidCountry(s string) bool { return countryRe.MatchString(s) }

// Validate checks a decoded JSON object against the schema and returns one
// message per problem (path-qualified, sorted for stable output). A nil
// schema accepts anything.
func (s *Schema) Validate(v map[string]any) []string {
	if s == nil {
		return nil
	}
	return s.check(v, false)
}

// ValidateOpen is Validate with every (nested) schema treated as Open:
// undeclared fields are accepted, declared ones are still typed. Inputs are
// checked this way because unmapped payload fields pass through.
func (s *Schema) ValidateOpen(v map[string]any) []string {
	if s == nil {
		return nil
	}
	return s.check(v, true)
}

func (s *Schema) check(v map[string]any, open bool) []string {
	var errs []string
	s.validate("", v, &errs, open)
	sort.Strings(errs)
	return errs
}

// ValidateJSON decodes raw (which must be a JSON object; numbers are kept
// exact) and validates it.
func (s *Schema) ValidateJSON(raw json.RawMessage) []string {
	v, err := decodeObject(raw)
	if err != nil {
		return []string{err.Error()}
	}
	return s.Validate(v)
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("output is not a JSON object: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("output has trailing data after the JSON object")
	}
	if v == nil {
		return nil, fmt.Errorf("output is null, want a JSON object")
	}
	return v, nil
}

func (s *Schema) validate(path string, v map[string]any, errs *[]string, open bool) {
	for _, f := range s.Fields {
		val, ok := v[f.Name]
		if !ok || val == nil {
			if f.Required {
				*errs = append(*errs, fmt.Sprintf("%s: required", join(path, f.Name)))
			}
			continue
		}
		f.check(join(path, f.Name), val, errs, open)
	}
	if s.Open || open {
		return
	}
	for k := range v {
		if strings.HasPrefix(k, "x_") {
			continue
		}
		if _, ok := s.Field(k); !ok {
			*errs = append(*errs, fmt.Sprintf("%s: unknown field (extensions must be prefixed x_)", join(path, k)))
		}
	}
}

func (f Field) check(path string, val any, errs *[]string, open bool) {
	bad := func(want string) {
		*errs = append(*errs, fmt.Sprintf("%s: want %s, got %s", path, want, describe(val)))
	}
	switch f.Type {
	case TypeAny:
	case TypeString:
		s, ok := val.(string)
		if !ok {
			bad("string")
			return
		}
		if len(f.Enum) > 0 && !contains(f.Enum, s) {
			*errs = append(*errs, fmt.Sprintf("%s: %q is not one of %s", path, s, strings.Join(f.Enum, "|")))
		}
		if f.Pattern != "" {
			if re, err := compilePattern(f.Pattern); err != nil {
				*errs = append(*errs, fmt.Sprintf("%s: contract pattern %q does not compile: %v", path, f.Pattern, err))
			} else if !re.MatchString(s) {
				*errs = append(*errs, fmt.Sprintf("%s: %q does not match %s", path, s, f.Pattern))
			}
		}
	case TypeDecimal:
		if s, ok := val.(string); !ok || !ValidDecimal(s) {
			bad(`decimal string (e.g. "199.90")`)
		}
	case TypeCurrency:
		if s, ok := val.(string); !ok || !currencyRe.MatchString(s) {
			bad("ISO 4217 currency code")
		}
	case TypeCountry:
		if s, ok := val.(string); !ok || !countryRe.MatchString(s) {
			bad("ISO 3166-1 alpha-2 country code")
		}
	case TypeInteger:
		if !isInteger(val) {
			bad("integer")
		}
	case TypeNumber:
		if !isNumber(val) {
			bad("number")
		}
	case TypeBoolean:
		if _, ok := val.(bool); !ok {
			bad("boolean")
		}
	case TypeObject:
		m, ok := val.(map[string]any)
		if !ok {
			bad("object")
			return
		}
		if f.Of != nil {
			f.Of.validate(path, m, errs, open)
		}
	case TypeArray:
		arr, ok := val.([]any)
		if !ok {
			bad("array")
			return
		}
		for i, el := range arr {
			ep := fmt.Sprintf("%s[%d]", path, i)
			if f.Of != nil {
				m, ok := el.(map[string]any)
				if !ok {
					*errs = append(*errs, fmt.Sprintf("%s: want object, got %s", ep, describe(el)))
					continue
				}
				f.Of.validate(ep, m, errs, open)
				continue
			}
			Field{Type: f.Elem, Enum: f.Enum, Pattern: f.Pattern}.check(ep, el, errs, open)
		}
	default:
		*errs = append(*errs, fmt.Sprintf("%s: contract declares unknown type %q", path, f.Type))
	}
}

func isNumber(v any) bool {
	switch v.(type) {
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32:
		return true
	}
	return false
}

func isInteger(v any) bool {
	switch n := v.(type) {
	case json.Number:
		_, err := n.Int64()
		return err == nil
	case float64:
		return n == float64(int64(n))
	case float32:
		return n == float32(int64(n))
	case int, int64, int32, uint, uint64, uint32:
		return true
	}
	return false
}

func describe(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	if isNumber(v) {
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// patterns caches compiled Field.Pattern expressions (pattern -> *regexp.Regexp).
var patterns sync.Map

func compilePattern(p string) (*regexp.Regexp, error) {
	if re, ok := patterns.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	patterns.Store(p, re)
	return re, nil
}

// checkPatterns reports the first Field.Pattern of s (nested included) that
// does not compile.
func (s *Schema) checkPatterns() error {
	if s == nil {
		return nil
	}
	for _, f := range s.Fields {
		if f.Pattern != "" {
			if _, err := compilePattern(f.Pattern); err != nil {
				return fmt.Errorf("field %q: pattern %q: %v", f.Name, f.Pattern, err)
			}
		}
		if err := f.Of.checkPatterns(); err != nil {
			return fmt.Errorf("field %q: %w", f.Name, err)
		}
	}
	return nil
}
