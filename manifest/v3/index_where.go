package v3

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	whereIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	whereAndRe   = regexp.MustCompile(`(?i)\s+AND\s+`)
	whereNullRe  = regexp.MustCompile(`(?i)^([a-z_][a-z0-9_]*)\s+IS\s+(NOT\s+)?NULL$`)
	whereCmpRe   = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\s*(<>|!=|>=|<=|=|>|<)\s*(.+)$`)
	whereStrRe   = regexp.MustCompile(`^'([A-Za-z0-9 _.:/-]*)'$`)
	whereNumRe   = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
)

// ParseIndexWhere validates a partial-index predicate (Index.Where) and renders
// it as SQL with every identifier quoted. The grammar is deliberately tiny — an
// AND-joined conjunction of `<column> IS [NOT] NULL` and `<column> <op> <literal>`
// (op: = <> != < <= > >=; literal: 'text', number, true, false) — so the string
// can never carry anything but a row filter into DDL. `cols`, when non-nil, is
// the set of columns the identifiers must belong to.
func ParseIndexWhere(expr string, cols map[string]struct{}) (string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", fmt.Errorf("empty predicate")
	}
	var out []string
	for _, term := range whereAndRe.Split(expr, -1) {
		term = strings.TrimSpace(term)
		var col, sql string
		if m := whereNullRe.FindStringSubmatch(term); m != nil {
			col = m[1]
			neg := ""
			if m[2] != "" {
				neg = "NOT "
			}
			sql = fmt.Sprintf(`%q IS %sNULL`, col, neg)
		} else if m := whereCmpRe.FindStringSubmatch(term); m != nil {
			col = m[1]
			op, lit := m[2], strings.TrimSpace(m[3])
			if op == "!=" {
				op = "<>"
			}
			switch {
			case whereStrRe.MatchString(lit), whereNumRe.MatchString(lit):
			case strings.EqualFold(lit, "true"), strings.EqualFold(lit, "false"):
				lit = strings.ToLower(lit)
			default:
				return "", fmt.Errorf("term %q: literal must be 'text', a number, true or false", term)
			}
			sql = fmt.Sprintf(`%q %s %s`, col, op, lit)
		} else {
			return "", fmt.Errorf("term %q: expected `<column> IS [NOT] NULL` or `<column> <op> <literal>`", term)
		}
		if cols != nil {
			if _, ok := cols[col]; !ok {
				return "", fmt.Errorf("term %q: %q is not a declared column on the model", term, col)
			}
		}
		out = append(out, sql)
	}
	return strings.Join(out, " AND "), nil
}
