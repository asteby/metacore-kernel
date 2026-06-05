package query

import (
	"regexp"
	"strings"
)

// FilterOp enumerates the filter operators the builder understands. Apps
// layering on extra operators (e.g. JSON path traversal) should do so in
// their own package via additional .Where clauses after Apply — adding
// operators here is a MAJOR bump because the wire protocol is stable.
type FilterOp string

// Supported operator constants. The string value is the wire token a
// client sends in f_<col>=<op>:<value>.
const (
	OpEq    FilterOp = "eq"
	OpIlike FilterOp = "ilike"
	OpIn    FilterOp = "in"
	OpGte   FilterOp = "gte"
	OpLte   FilterOp = "lte"
	OpRange FilterOp = "range"
)

// Extended operator constants used by the ops filter dialect (see
// dialect.go / ParseOpsFilters). These are ADDITIVE: the default parser
// (ParseFromMap) never emits them, so existing consumers see no change.
// applyOneFilter dispatches on them; an op a given switch does not handle
// degrades to a dropped clause (the kernel's "garbage in → safe degrade"
// policy), never a panic.
const (
	// OpNeq is `<col> <> ?`. Value: string. The SDK emits `neq` as a
	// first-class operator; without it not-equal filters degraded to a
	// literal exact-match on the whole "neq:<val>" token.
	OpNeq FilterOp = "neq"
	// OpNotIn is `<col> NOT IN ?`. Value: []string.
	OpNotIn FilterOp = "not_in"
	// OpLike is a CASE-SENSITIVE `<col> LIKE ? ESCAPE '\'` wrapped in
	// %...%. Value: string. Distinct from OpIlike which is case- and
	// accent-insensitive via unaccent().
	OpLike FilterOp = "like"
	// OpGt is `<col> > ?` with a numeric (float64) value.
	OpGt FilterOp = "gt"
	// OpLt is `<col> < ?` with a numeric (float64) value.
	OpLt FilterOp = "lt"
	// OpNumGte is `<col> >= ?` with a numeric (float64) value. Distinct
	// from OpGte (string), which ops does not use for the GTE operator.
	OpNumGte FilterOp = "num_gte"
	// OpNumLte is `<col> <= ?` with a numeric (float64) value.
	OpNumLte FilterOp = "num_lte"
	// OpNumRange is `<col> >= ? [AND <col> <= ?]` over a numeric range.
	// Value: [2]*float64 (nil side omitted).
	OpNumRange FilterOp = "num_range"
	// OpUnaccentIlike is the ops ILIKE: accent- and case-insensitive via
	// `unaccent(<col>) ILIKE unaccent(?) ESCAPE '\'`. Value: string.
	OpUnaccentIlike FilterOp = "unaccent_ilike"
	// OpNull is `<col> IS NULL`. Value is ignored.
	OpNull FilterOp = "null"
	// OpNotNull is `<col> IS NOT NULL`. Value is ignored.
	OpNotNull FilterOp = "not_null"
	// OpDateRange is `<col> >= ? AND <col> <= ?` over two time.Time
	// bounds (end snapped to 23:59:59). Value: [2]time.Time.
	OpDateRange FilterOp = "date_range"
	// OpJSONBEq is `<jsonbcol>->>? = ?` exact match on a JSONB key.
	// Value: JSONBFilter{Key, Val}. The bound column carries the JSONB
	// column name; the key/value are bound as parameters.
	OpJSONBEq FilterOp = "jsonb_eq"
)

// JSONBFilter is the Value type for OpJSONBEq. Key is the JSON object key
// extracted with the `->>` operator; Val is the literal compared for
// equality. Both are passed as bound parameters (no interpolation), so a
// JSONB filter is injection-safe even though the path bypasses the
// column whitelist.
type JSONBFilter struct {
	Key string
	Val string
}

// Filter is a parsed f_<col>=<op>:<value> directive. Value is typed per Op:
//
//	OpEq, OpIlike, OpGte, OpLte: string
//	OpIn:                        []string
//	OpRange:                     [2]string {min, max} — either side may be ""
//
// Apps that peek into Filter should use a type switch — the concrete Go
// type of Value is part of the stable API.
type Filter struct {
	Op    FilterOp
	Value interface{}
}

// parseFilterValue decodes the "<op>:<value>" right-hand side of an f_
// parameter. Values with no ":" default to OpEq. Unknown operators fall
// back to OpEq with the whole string as the value (a defensive choice:
// we'd rather match literally than silently drop a filter the client
// believed was applied).
func parseFilterValue(raw string) Filter {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Filter{Op: OpEq, Value: ""}
	}

	idx := strings.Index(raw, ":")
	if idx < 0 {
		return Filter{Op: OpEq, Value: raw}
	}

	op := FilterOp(strings.ToLower(raw[:idx]))
	val := raw[idx+1:]

	switch op {
	case OpEq, OpIlike, OpGte, OpLte:
		return Filter{Op: op, Value: val}
	case OpIn:
		parts := strings.Split(val, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return Filter{Op: OpIn, Value: out}
	case OpRange:
		parts := strings.SplitN(val, "|", 2)
		rng := [2]string{}
		if len(parts) >= 1 {
			rng[0] = strings.TrimSpace(parts[0])
		}
		if len(parts) == 2 {
			rng[1] = strings.TrimSpace(parts[1])
		}
		return Filter{Op: OpRange, Value: rng}
	default:
		// Unknown operator → treat entire string as literal eq value.
		return Filter{Op: OpEq, Value: raw}
	}
}

// identRe matches a safe SQL identifier (column name). Matches the
// canonical PostgreSQL unquoted identifier rule, which is more
// restrictive than SQL standard but sufficient for every table the
// kernel owns. Use this BEFORE interpolating any column name into raw
// SQL that bypasses GORM's placeholder binder.
var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// isSafeIdent returns true when s can be safely interpolated as a
// column name in raw SQL. The check is deliberately stricter than the
// SQL grammar: the kernel owns the column names, and anything weird is
// a bug upstream.
func isSafeIdent(s string) bool {
	return identRe.MatchString(s)
}
