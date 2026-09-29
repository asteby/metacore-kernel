package modelbase

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Validatable is the optional interface a Go model implements to validate
// itself before the dynamic CRUD writes it. The dynamic service calls
// Validate after the input is merged into the model (on update: the persisted
// row plus the changes) and before the INSERT/UPDATE. A non-empty map aborts
// the write with 422 and `{errors: {<field>: [{code: "invalid", message}]}}`.
//
//	func (p *Promotion) Validate() map[string]string {
//	    errs := map[string]string{}
//	    if p.EndsAt.Before(p.StartsAt) {
//	        errs["ends_at"] = "must be after starts_at"
//	    }
//	    return errs
//	}
type Validatable interface {
	Validate() map[string]string
}

// WriteValidator is the context-aware variant of Validatable. op is "create"
// or "update". Return FieldErrors (or a *dynamic.ValidationError) for a 422
// with a per-field map; any other error aborts the write unchanged.
type WriteValidator interface {
	ValidateWrite(ctx context.Context, op string) error
}

// FieldErrors is a field → message map usable as an error. Returned from a
// hook or a WriteValidator, the dynamic handler answers 422 with the same
// per-field shape as the declarative validation.
type FieldErrors map[string]string

// Error implements error.
func (f FieldErrors) Error() string {
	if len(f) == 0 {
		return "validation failed"
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", k, f[k]))
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Err returns f as an error, or nil when f is empty. Use it to avoid
// returning a non-nil error that holds an empty map.
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	return f
}

// Range returns a ValidationRule bounding a value to [min, max] — numeric
// value for number fields, length for text (see package validate).
func Range(min, max float64) *ValidationRule {
	return &ValidationRule{Min: &min, Max: &max}
}

// MinValue returns a ValidationRule with only a lower bound.
func MinValue(min float64) *ValidationRule { return &ValidationRule{Min: &min} }

// MaxValue returns a ValidationRule with only an upper bound.
func MaxValue(max float64) *ValidationRule { return &ValidationRule{Max: &max} }

// Pattern returns a ValidationRule requiring the value to match regex.
func Pattern(regex string) *ValidationRule { return &ValidationRule{Regex: regex} }

// WithPattern returns a copy of r that also requires regex.
func (r ValidationRule) WithPattern(regex string) *ValidationRule {
	r.Regex = regex
	return &r
}

// WithCustom returns a copy of r that also runs the named custom validator.
func (r ValidationRule) WithCustom(slug string) *ValidationRule {
	r.Custom = slug
	return &r
}
