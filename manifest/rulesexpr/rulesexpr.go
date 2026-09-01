// Package rulesexpr parses and evaluates the `when` predicate of a declarative
// rule (contributions.rules[].when — see manifest/v3.RuleDef): an arithmetic
// comparison such as `amount_paid + amount_due != total` or
// `qty_on_hand < reorder_point`. It does not invent a third expression
// grammar: both sides of the comparison are arithmetic expressions parsed and
// evaluated by the EXISTING computeexpr engine (manifest/computeexpr — the
// same one backing rollup/formula columns), so the security boundary (strict
// allowlisted lexer, no function calls, no SQL) is identical. rulesexpr only
// adds the comparison operator on top.
package rulesexpr

import (
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/manifest/computeexpr"
)

// Op is a comparison operator.
type Op string

const (
	OpEq Op = "=="
	OpNe Op = "!="
	OpLt Op = "<"
	OpLe Op = "<="
	OpGt Op = ">"
	OpGe Op = ">="
)

// orderedOps lists the recognised operators, LONGEST FIRST so the scanner
// never mistakes the first byte of a 2-char operator (==, !=, <=, >=) for a
// 1-char one (<, >).
var orderedOps = []Op{OpEq, OpNe, OpLe, OpGe, OpLt, OpGt}

// Rule is a parsed `when` predicate: left <op> right, where left and right
// are computeexpr arithmetic expressions.
type Rule struct {
	src         string
	left, right string
	op          Op
	fields      []string
}

// Parse parses src ("left op right"). Both sides must be valid computeexpr
// arithmetic expressions (numbers, identifiers, + - * / and parentheses) and
// exactly one top-level comparison operator must separate them.
func Parse(src string) (*Rule, error) {
	trimmed := strings.TrimSpace(src)
	if trimmed == "" {
		return nil, fmt.Errorf("empty rule expression")
	}
	left, op, right, err := splitOnOp(trimmed)
	if err != nil {
		return nil, err
	}
	if err := computeexpr.Validate(left, nil); err != nil {
		return nil, fmt.Errorf("left side %q: %w", left, err)
	}
	if err := computeexpr.Validate(right, nil); err != nil {
		return nil, fmt.Errorf("right side %q: %w", right, err)
	}
	r := &Rule{src: trimmed, left: left, right: right, op: op}
	r.fields = collectFields(left, right)
	return r, nil
}

// Fields returns the sorted set of identifiers referenced by either side —
// used by manifest validators to check every column exists on the model.
func (r *Rule) Fields() []string {
	if r == nil {
		return nil
	}
	return r.fields
}

// Eval evaluates the rule against record (column name → value). Missing
// columns evaluate to 0, matching computeexpr's ERP-safe default.
func (r *Rule) Eval(record map[string]any) (bool, error) {
	if r == nil {
		return false, fmt.Errorf("nil rule")
	}
	env := make(map[string]float64, len(record))
	for k, v := range record {
		env[k] = computeexpr.ToFloat(v)
	}
	lv, err := computeexpr.Eval(r.left, env)
	if err != nil {
		return false, err
	}
	rv, err := computeexpr.Eval(r.right, env)
	if err != nil {
		return false, err
	}
	switch r.op {
	case OpEq:
		return lv == rv, nil
	case OpNe:
		return lv != rv, nil
	case OpLt:
		return lv < rv, nil
	case OpLe:
		return lv <= rv, nil
	case OpGt:
		return lv > rv, nil
	case OpGe:
		return lv >= rv, nil
	}
	return false, fmt.Errorf("unknown operator %q", r.op)
}

// splitOnOp scans src for the first top-level comparison operator (outside
// parentheses) and splits on it. It rejects a src with zero or more than one
// top-level operator.
func splitOnOp(src string) (left string, op Op, right string, err error) {
	depth := 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch c {
		case '(':
			depth++
			continue
		case ')':
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		for _, candidate := range orderedOps {
			s := string(candidate)
			if strings.HasPrefix(src[i:], s) {
				left = strings.TrimSpace(src[:i])
				right = strings.TrimSpace(src[i+len(s):])
				if left == "" || right == "" {
					return "", "", "", fmt.Errorf("expression %q: missing operand around %q", src, s)
				}
				// Ensure no SECOND top-level operator remains — reject chained
				// comparisons like `a < b < c` which arithmetic evaluation
				// cannot express (their meaning would be ambiguous).
				if _, _, _, err2 := splitOnOp(right); err2 == nil {
					return "", "", "", fmt.Errorf("expression %q: only one comparison operator is allowed", src)
				}
				return left, candidate, right, nil
			}
		}
	}
	return "", "", "", fmt.Errorf("expression %q: expected exactly one comparison operator (== != < <= > >=)", src)
}

func collectFields(left, right string) []string {
	set := map[string]struct{}{}
	for _, side := range []string{left, right} {
		for _, tok := range splitIdents(side) {
			set[tok] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// splitIdents extracts bare identifiers from an arithmetic expression string
// without re-implementing the lexer: computeexpr.Validate already rejects
// anything illegal, so a light regex-free scan here is enough to recover the
// identifier tokens for Fields().
func splitIdents(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tok := cur.String()
		cur.Reset()
		if tok == "" {
			return
		}
		c0 := tok[0]
		if c0 >= '0' && c0 <= '9' {
			return // numeric literal, not an identifier
		}
		out = append(out, tok)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isIdentChar := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.'
		if isIdentChar {
			cur.WriteByte(c)
		} else {
			flush()
		}
	}
	flush()
	return out
}
