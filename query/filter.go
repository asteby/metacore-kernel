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
