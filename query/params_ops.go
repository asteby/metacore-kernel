package query

import (
	"strconv"
	"strings"
)

// ParseOpsFromValues parses request parameters using the ops filter
// dialect. It is the additive counterpart to ParseFromMap: same canonical
// pagination/sort/search keys (page, per_page, sortBy, order, search), but
// the `f_<col>` values are decoded with ParseOpsFilterValue, and the ops
// JSONB / relation / native-array conventions are honoured.
//
// It is fully transport-agnostic and never imports Fiber (Law 3). A host
// wires it the same way as ParseFromMap; nothing in the default path
// changes, so existing consumers keep ParseFromMap and are unaffected.
//
// Differences from ParseFromMap:
//
//   - f_<col> values use the ops dialect (IN/NOT_IN/LIKE/ILIKE/GT/LT/GTE/
//     LTE/RANGE/NULL/NOT_NULL/date-range). See ParseOpsFilterValue.
//   - f_fiscal_data.<key>=v becomes a JSONB equality filter (OpJSONBEq).
//   - f_<rel>.<field> becomes a RelationFilter (same as ParseFromMap).
//   - a value that arrives as a native string slice (len > 1, e.g. repeated
//     query params or a JSON array) is treated as an IN filter directly.
//
// The `page` parameter is read as a string (strconv.Atoi), defaulting to 1,
// matching the ops handler.
func ParseOpsFromValues(values map[string][]string) (Params, error) {
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

		// Native multi-value (repeated param) → IN filter, no operator
		// syntax. Mirrors the ops []interface{} array branch.
		nonEmpty := make([]string, 0, len(vs))
		for _, v := range vs {
			if v != "" {
				nonEmpty = append(nonEmpty, v)
			}
		}
		if len(nonEmpty) > 1 {
			if isSafeIdent(col) {
				p.Filters[col] = Filter{Op: OpIn, Value: nonEmpty}
			}
			continue
		}
		if len(nonEmpty) == 0 {
			continue
		}
		raw := nonEmpty[0]

		// JSONB path: f_fiscal_data.<key> → OpJSONBEq. Handled BEFORE the
		// generic dot-promotion so a JSONB key with a dot is not mistaken
		// for a relation. Bypasses isSafeIdent on the key (passed as a
		// bound parameter, never interpolated).
		if strings.HasPrefix(col, jsonbColumn+".") {
			jsonKey := strings.TrimPrefix(col, jsonbColumn+".")
			if jsonKey != "" && raw != "" {
				p.Filters[jsonbColumn] = Filter{
					Op:    OpJSONBEq,
					Value: JSONBFilter{Key: jsonKey, Val: raw},
				}
			}
			continue
		}

		// Relation filter: f_<rel>.<field>. Split on the first dot.
		if dot := strings.Index(col, "."); dot > 0 && dot < len(col)-1 {
			rel := col[:dot]
			field := col[dot+1:]
			if isSafeIdent(rel) && isSafeIdent(field) {
				f := ParseOpsFilterValue(raw)
				p.RelationFilters = append(p.RelationFilters, RelationFilter{
					Relation: rel,
					Field:    field,
					Op:       f.Op,
					Value:    f.Value,
				})
			}
			continue
		}

		p.Filters[col] = ParseOpsFilterValue(raw)
	}

	if v, ok := firstNonEmpty(values, "group_by"); ok {
		p.GroupBy = parseCSV(v)
	}
	if v, ok := firstNonEmpty(values, "with"); ok {
		p.Preloads = parseCSV(v)
	}

	return p, nil
}
