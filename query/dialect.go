package query

import (
	"strconv"
	"strings"
	"time"
)

// ParseFilterValue is the function signature for a per-value filter
// dialect. Given the raw right-hand side of an `f_<col>=...` parameter it
// returns the parsed Filter. A dialect never sees the column key — column
// promotion (flat vs relation vs JSONB) is decided by the parser that
// drives the dialect (see ParseOpsFilters), keeping the dialect a pure
// value decoder.
//
// The default dialect is parseFilterValue (filter.go); the ops dialect is
// ParseOpsFilterValue (this file). A host can supply its own by writing a
// parser that calls it — the Builder consumes Params, never the dialect.
type ParseFilterValue func(raw string) Filter

// dateRangeLayout is the date layout the ops DATE-RANGE shorthand uses:
// `YYYY-MM-DD_YYYY-MM-DD` (underscore separator, NOT colon). Each side is
// a 10-char date; the end is snapped to 23:59:59 of that day.
const dateRangeLayout = "2006-01-02"

// MaxFilterValueLength caps the accepted filter value length, matching the
// ops sanitiser. Values longer than this are truncated before parsing.
const MaxFilterValueLength = 255

// ParseOpsFilterValue decodes one flat-column filter value using the ops
// wire dialect. It is ADDITIVE — the default ParseFromMap never calls it,
// so existing consumers are unaffected. Supported forms:
//
//	value                  → OpEq (exact, no operator)
//	IN:a,b,c               → OpNotIn / OpIn (comma list)
//	NOT_IN:a,b,c           → OpNotIn
//	LIKE:txt               → OpLike (case-sensitive, wrapped %txt%)
//	ILIKE:txt              → OpUnaccentIlike (accent+case insensitive)
//	GT:10 LT:10 GTE LTE    → numeric comparison (float64; non-numeric → no-op)
//	RANGE:min,max          → OpNumRange (either side optional)
//	NULL / NULL:           → OpNull
//	NOT_NULL / NOT_NULL:   → OpNotNull
//	YYYY-MM-DD_YYYY-MM-DD  → OpDateRange (underscore separator)
//
// The operator token (before the first ':') is case-insensitive
// (strings.ToUpper). An operator the dialect does not recognise falls back
// to OpEq with the whole string as the value — matching the default
// dialect's defensive "match literally" stance.
func ParseOpsFilterValue(raw string) Filter {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Filter{Op: OpEq, Value: ""}
	}
	if len(raw) > MaxFilterValueLength {
		raw = raw[:MaxFilterValueLength]
	}
	raw = stripControl(raw)

	// Bare NULL / NOT_NULL with no ':' — accepted as a convenience even
	// though the audited ops code path required the ':'. This is the
	// intuitive behaviour and avoids the `deleted_at = 'NULL'` footgun
	// documented in the spec.
	switch strings.ToUpper(raw) {
	case "NULL":
		return Filter{Op: OpNull}
	case "NOT_NULL":
		return Filter{Op: OpNotNull}
	}

	// DATE RANGE shorthand: two 10-char dates joined by '_'. Detected
	// before the ':' operator split because it uses '_', not ':'.
	if f, ok := parseDateRange(raw); ok {
		return f
	}

	idx := strings.Index(raw, ":")
	if idx < 0 {
		// No operator → exact match.
		return Filter{Op: OpEq, Value: raw}
	}

	op := strings.ToUpper(strings.TrimSpace(raw[:idx]))
	arg := raw[idx+1:]

	switch op {
	case "EQ":
		// Explicit exact-match operator. This is the SDK's DEFAULT operator —
		// every column filter, relation FK filter and polymorphic scope param is
		// emitted as `f_<col>=eq:<val>` (see runtime-react dynamic-table /
		// dynamic-relation-helpers). Without this case `eq:10` fell through to
		// the `default` branch and exact-matched the WHOLE literal "eq:10",
		// which silently never matched on text columns and 500'd on numeric ones
		// (`invalid input syntax for type numeric: "eq:10"`). Strip the operator
		// and match on the bare argument.
		return Filter{Op: OpEq, Value: arg}
	case "NEQ", "NE":
		// Not-equal. The SDK exposes `neq` as a first-class operator
		// (runtime-react types FilterOperator) but the dialect never decoded it,
		// so it degraded to the same literal-match footgun as EQ.
		return Filter{Op: OpNeq, Value: arg}
	case "IN":
		return Filter{Op: OpIn, Value: splitCSVNonEmpty(arg)}
	case "NOT_IN":
		return Filter{Op: OpNotIn, Value: splitCSVNonEmpty(arg)}
	case "LIKE":
		return Filter{Op: OpLike, Value: arg}
	case "ILIKE", "CONTAINS":
		// CONTAINS is the SDK runtime's internal operator name for an accent/
		// case-insensitive substring match. The client SHOULD map it to the
		// ILIKE alias, but accept the raw name too so a text filter works either
		// way — an unrecognised operator silently exact-matched the whole
		// "contains:foo" literal, so the filter appeared to do nothing.
		return Filter{Op: OpUnaccentIlike, Value: arg}
	case "GT":
		if n, ok := parseFloat(arg); ok {
			return Filter{Op: OpGt, Value: n}
		}
		return noopFilter()
	case "LT":
		if n, ok := parseFloat(arg); ok {
			return Filter{Op: OpLt, Value: n}
		}
		return noopFilter()
	case "GTE":
		if n, ok := parseFloat(arg); ok {
			return Filter{Op: OpNumGte, Value: n}
		}
		return noopFilter()
	case "LTE":
		if n, ok := parseFloat(arg); ok {
			return Filter{Op: OpNumLte, Value: n}
		}
		return noopFilter()
	case "RANGE":
		return parseNumRange(arg)
	case "NULL":
		return Filter{Op: OpNull}
	case "NOT_NULL":
		return Filter{Op: OpNotNull}
	default:
		// Unknown operator → literal exact match on the WHOLE string,
		// mirroring the default dialect.
		return Filter{Op: OpEq, Value: raw}
	}
}

// noopFilter returns a Filter whose Op no switch handles, so applyOneFilter
// drops it. Used when an operator's argument fails to parse (e.g. GT on a
// non-numeric value) — the ops behaviour is "log + skip", and the kernel
// equivalent is a silently-dropped clause.
func noopFilter() Filter {
	return Filter{Op: FilterOp("noop")}
}

// parseDateRange recognises the `YYYY-MM-DD_YYYY-MM-DD` shorthand. Returns
// ok=false when the value does not look like a date range so the caller
// can fall through to operator parsing.
func parseDateRange(raw string) (Filter, bool) {
	// Match ops semantics exactly: split on "_" and require EXACTLY two parts,
	// each a full YYYY-MM-DD date. A value with extra underscores (e.g.
	// "2024-01-01_2024-03-31_x") is NOT a date range — the caller falls through
	// to operator / literal-eq parsing, identical to ops query_sorting.go.
	parts := strings.Split(raw, "_")
	if len(parts) != 2 {
		return Filter{}, false
	}
	start, err := time.Parse(dateRangeLayout, parts[0])
	if err != nil {
		return Filter{}, false
	}
	end, err := time.Parse(dateRangeLayout, parts[1])
	if err != nil {
		return Filter{}, false
	}
	// Snap end to end-of-day so a single-day range still matches rows
	// timestamped during that day.
	end = time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 0, end.Location())
	return Filter{Op: OpDateRange, Value: [2]time.Time{start, end}}, true
}

// parseNumRange decodes `RANGE:min,max`. Match ops semantics exactly: a comma
// is REQUIRED (ops needs len(parts)==2) — `RANGE:5` without a comma is dropped,
// NOT treated as `>= 5`. Either side may be empty (`RANGE:5,` → >=5;
// `RANGE:,10` → <=10). Non-numeric sides are dropped; if neither parses it's a
// no-op.
func parseNumRange(arg string) Filter {
	parts := strings.SplitN(arg, ",", 2)
	if len(parts) != 2 {
		return noopFilter()
	}
	var rng [2]*float64
	if n, ok := parseFloat(strings.TrimSpace(parts[0])); ok {
		rng[0] = &n
	}
	if n, ok := parseFloat(strings.TrimSpace(parts[1])); ok {
		rng[1] = &n
	}
	if rng[0] == nil && rng[1] == nil {
		return noopFilter()
	}
	return Filter{Op: OpNumRange, Value: rng}
}

func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func splitCSVNonEmpty(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stripControl removes ASCII control characters (except none are kept) from
// s while preserving accents/ñ/unicode letters, matching the ops
// sanitiseFilterValue behaviour.
func stripControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// parseWithDialect parses request parameters with the same structure as
// ParseFromMap (pagination/sort/search keys, relation-filter dot promotion)
// but decodes each flat `f_<col>` value through the supplied dialect rather
// than the built-in parseFilterValue. This is the hook used by
// WithFilterDialect for hosts that want a custom value syntax without the
// full ops JSONB/native-array promotions.
func parseWithDialect(values map[string][]string, decode ParseFilterValue) (Params, error) {
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
		var raw string
		for _, v := range vs {
			if v != "" {
				raw = v
				break
			}
		}
		if raw == "" {
			continue
		}
		if dot := strings.Index(col, "."); dot > 0 && dot < len(col)-1 {
			rel := col[:dot]
			field := col[dot+1:]
			if isSafeIdent(rel) && isSafeIdent(field) {
				f := decode(raw)
				p.RelationFilters = append(p.RelationFilters, RelationFilter{
					Relation: rel, Field: field, Op: f.Op, Value: f.Value,
				})
			}
			continue
		}
		p.Filters[col] = decode(raw)
	}
	if v, ok := firstNonEmpty(values, "group_by"); ok {
		p.GroupBy = parseCSV(v)
	}
	if v, ok := firstNonEmpty(values, "with"); ok {
		p.Preloads = parseCSV(v)
	}
	return p, nil
}

// jsonbColumn is the single JSONB column the ops dialect special-cases.
// Matches the audited ops behaviour where only `fiscal_data` is treated as
// a JSONB document; every other column is scalar. Hosts that need a
// different JSONB column can build Params directly with an OpJSONBEq
// Filter.
const jsonbColumn = "fiscal_data"
