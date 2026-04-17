package query

import (
	"fmt"
	"strconv"
	"strings"
)

// Params is the normalized result of parsing a request's query string.
// All fields have safe defaults — zero values are valid and produce the
// documented defaults when fed into Builder methods.
type Params struct {
	// Page is 1-indexed. Defaults to DefaultPage.
	Page int

	// PerPage is clamped to [1, MaxPerPage]. Zero → DefaultPerPage.
	PerPage int

	// SortBy is the column name to order by. Must appear in the bound
	// TableMetadata.Columns — otherwise Builder drops it silently.
	SortBy string

	// Order is "asc" or "desc" (lowercase). Anything else falls back to
	// "desc" inside Builder.
	Order string

	// Search is the free-text search string. Applied against every
	// TableMetadata.SearchColumns entry via ILIKE OR ILIKE OR … .
	Search string

	// Filters is keyed by column name. Keys are whitelisted against
	// TableMetadata.Columns at apply time; unknown keys are dropped.
	Filters map[string]Filter
}

// ParseFromMap parses a url.Values-style map into Params. It accepts the
// exact parameter names used historically by the ops/link frontends:
//
//	page, per_page, sortBy, order, search, f_<col>
//
// It is intentionally lenient: malformed page/per_page values fall back
// to defaults rather than returning errors, which matches the audited
// behaviour of the source services. The returned error is non-nil only
// for structural problems that no reasonable default can paper over.
func ParseFromMap(values map[string][]string) (Params, error) {
	p := Params{
		Page:    DefaultPage,
		PerPage: DefaultPerPage,
		Filters: map[string]Filter{},
	}

	if v, ok := firstNonEmpty(values, "page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Page = n
		}
	}

	if v, ok := firstNonEmpty(values, "per_page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.PerPage = n
		}
	}
	if p.PerPage > MaxPerPage {
		p.PerPage = MaxPerPage
	}

	if v, ok := firstNonEmpty(values, "sortBy"); ok {
		p.SortBy = strings.TrimSpace(v)
	}

	if v, ok := firstNonEmpty(values, "order"); ok {
		o := strings.ToLower(strings.TrimSpace(v))
		if o == "asc" || o == "desc" {
			p.Order = o
		}
	}

	if v, ok := firstNonEmpty(values, "search"); ok {
		s := strings.TrimSpace(v)
		if len(s) > MaxSearchTermLength {
			s = s[:MaxSearchTermLength]
		}
		p.Search = s
	}

	for key, vs := range values {
		if !strings.HasPrefix(key, "f_") {
			continue
		}
		col := strings.TrimPrefix(key, "f_")
		if col == "" || len(vs) == 0 {
			continue
		}
		// Take first non-empty occurrence.
		for _, v := range vs {
			if v == "" {
				continue
			}
			p.Filters[col] = parseFilterValue(v)
			break
		}
	}

	if p.Page < 1 {
		return p, fmt.Errorf("%w: page must be >= 1", ErrInvalidParam)
	}
	return p, nil
}

// firstNonEmpty returns the first non-empty value for key in values, or
// the empty string and false if the key is absent or every slot is
// empty. Callers use the second return to distinguish "unset" from
// "set to empty string".
func firstNonEmpty(values map[string][]string, key string) (string, bool) {
	vs, ok := values[key]
	if !ok {
		return "", false
	}
	for _, v := range vs {
		if v != "" {
			return v, true
		}
	}
	return "", false
}
