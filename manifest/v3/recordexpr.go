package v3

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RecordExpr is a parsed record predicate — the value of
// PublicRoute.EnabledWhen — evaluated by hosts against a record map
// (column name → JSON-decoded value) to decide whether a contribution applies
// to that record.
//
// Grammar (whitespace-insensitive):
//
//	expr    := or
//	or      := and ( "||" and )*
//	and     := unit ( "&&" unit )*
//	unit    := "(" expr ")" | cmp
//	cmp     := path op literal | path ("in" | "not_in" | "not in") list
//	op      := "==" | "=" | "!=" | "<>" | "<" | "<=" | ">" | ">="
//	literal := 'string' | "string" | number | true | false | null
//	list    := "(" literal ("," literal)* ")" | "[" literal ("," literal)* "]"
//	path    := [a-z_][a-z0-9_]* ( "." [a-z_][a-z0-9_]* )*
//
// Semantics: a comparison against a number is numeric when the record value
// parses as a number; against true/false it is boolean (record booleans and
// the strings "true"/"false"/"1"/"0" coerce); `== null` holds when the value
// is missing, nil or the empty string; everything else compares as trimmed
// strings (so `<`/`>` on ISO dates order correctly). A missing column compares
// as "" — `status != 'draft'` therefore holds for a record with no status,
// which is the permissive default hosts expect from an optional gate.
//
// The zero RecordExpr (or one parsed from an empty string) evaluates true.
type RecordExpr struct {
	root   exprNode
	fields map[string]struct{}
}

// ParseRecordExpr parses s. An empty/blank s yields an expression that is
// always true. Any syntax error is returned with the offending token so a
// manifest validator can surface it verbatim.
func ParseRecordExpr(s string) (*RecordExpr, error) {
	e := &RecordExpr{fields: map[string]struct{}{}}
	if strings.TrimSpace(s) == "" {
		return e, nil
	}
	toks, err := lexRecordExpr(s)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks, expr: e}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.eof() {
		return nil, fmt.Errorf("unexpected %q", p.peek().text)
	}
	e.root = node
	return e, nil
}

// Fields returns the sorted set of top-level column names the expression
// reads (the first segment of every path), so validators can check they exist
// on the model.
func (e *RecordExpr) Fields() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.fields))
	for f := range e.fields {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Eval reports whether the predicate holds for record. A nil/empty expression
// is always true.
func (e *RecordExpr) Eval(record map[string]any) bool {
	if e == nil || e.root == nil {
		return true
	}
	return e.root.eval(record)
}

// IsEmpty reports whether the expression has no predicate (always true).
func (e *RecordExpr) IsEmpty() bool { return e == nil || e.root == nil }

// ---------------------------------------------------------------------------
// lexer

type exprTokKind int

const (
	tokIdent exprTokKind = iota
	tokString
	tokNumber
	tokOp     // == != < <= > >= = <>
	tokAnd    // &&
	tokOr     // ||
	tokLParen // (
	tokRParen // )
	tokLBrack // [
	tokRBrack // ]
	tokComma  // ,
)

type exprToken struct {
	kind exprTokKind
	text string
}

func lexRecordExpr(s string) ([]exprToken, error) {
	var toks []exprToken
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, exprToken{tokLParen, "("})
			i++
		case c == ')':
			toks = append(toks, exprToken{tokRParen, ")"})
			i++
		case c == '[':
			toks = append(toks, exprToken{tokLBrack, "["})
			i++
		case c == ']':
			toks = append(toks, exprToken{tokRBrack, "]"})
			i++
		case c == ',':
			toks = append(toks, exprToken{tokComma, ","})
			i++
		case c == '&':
			if i+1 < len(s) && s[i+1] == '&' {
				toks = append(toks, exprToken{tokAnd, "&&"})
				i += 2
				continue
			}
			return nil, fmt.Errorf("unexpected %q at %d (did you mean &&?)", string(c), i)
		case c == '|':
			if i+1 < len(s) && s[i+1] == '|' {
				toks = append(toks, exprToken{tokOr, "||"})
				i += 2
				continue
			}
			return nil, fmt.Errorf("unexpected %q at %d (did you mean ||?)", string(c), i)
		case c == '=' || c == '!' || c == '<' || c == '>':
			op := string(c)
			if i+1 < len(s) && (s[i+1] == '=' || (c == '<' && s[i+1] == '>')) {
				op += string(s[i+1])
			}
			switch op {
			case "==", "=", "!=", "<>", "<", "<=", ">", ">=":
				toks = append(toks, exprToken{tokOp, op})
				i += len(op)
			default:
				return nil, fmt.Errorf("unexpected operator %q at %d", op, i)
			}
		case c == '\'' || c == '"':
			quote := c
			j := i + 1
			var b strings.Builder
			closed := false
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == quote {
					closed = true
					break
				}
				b.WriteByte(s[j])
				j++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string starting at %d", i)
			}
			toks = append(toks, exprToken{tokString, b.String()})
			i = j + 1
		case (c >= '0' && c <= '9') || (c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			j := i + 1
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				j++
			}
			num := s[i:j]
			if _, err := strconv.ParseFloat(num, 64); err != nil {
				return nil, fmt.Errorf("invalid number %q at %d", num, i)
			}
			toks = append(toks, exprToken{tokNumber, num})
			i = j
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
			j := i + 1
			for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= '0' && s[j] <= '9') || s[j] == '_' || s[j] == '.') {
				j++
			}
			toks = append(toks, exprToken{tokIdent, s[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected %q at %d", string(c), i)
		}
	}
	return toks, nil
}

// ---------------------------------------------------------------------------
// parser

type exprParser struct {
	toks []exprToken
	pos  int
	expr *RecordExpr
}

func (p *exprParser) eof() bool       { return p.pos >= len(p.toks) }
func (p *exprParser) peek() exprToken { return p.toks[p.pos] }
func (p *exprParser) next() exprToken { t := p.toks[p.pos]; p.pos++; return t }
func (p *exprParser) accept(k exprTokKind, text string) bool {
	if p.eof() {
		return false
	}
	t := p.peek()
	if t.kind == k && (text == "" || t.text == text) {
		p.pos++
		return true
	}
	return false
}

func (p *exprParser) parseOr() (exprNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept(tokOr, "") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &orNode{left, right}
	}
	return left, nil
}

func (p *exprParser) parseAnd() (exprNode, error) {
	left, err := p.parseUnit()
	if err != nil {
		return nil, err
	}
	for p.accept(tokAnd, "") {
		right, err := p.parseUnit()
		if err != nil {
			return nil, err
		}
		left = &andNode{left, right}
	}
	return left, nil
}

func (p *exprParser) parseUnit() (exprNode, error) {
	if p.eof() {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	if p.accept(tokLParen, "") {
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.accept(tokRParen, "") {
			return nil, fmt.Errorf("expected ) to close group")
		}
		return inner, nil
	}
	return p.parseCmp()
}

func (p *exprParser) parseCmp() (exprNode, error) {
	t := p.next()
	if t.kind != tokIdent || !validExprPath(t.text) {
		return nil, fmt.Errorf("expected a column name, got %q", t.text)
	}
	path := t.text
	p.expr.fields[strings.SplitN(path, ".", 2)[0]] = struct{}{}
	if p.eof() {
		return nil, fmt.Errorf("expected an operator after %q", path)
	}
	op := p.next()
	switch {
	case op.kind == tokOp:
		lit, err := p.parseLiteral()
		if err != nil {
			return nil, err
		}
		return &cmpNode{path: path, op: normalizeOp(op.text), lit: lit}, nil
	case op.kind == tokIdent && (op.text == "in" || op.text == "not_in" || op.text == "not"):
		negate := op.text != "in"
		if op.text == "not" {
			if !p.accept(tokIdent, "in") {
				return nil, fmt.Errorf("expected `in` after `not`")
			}
		}
		list, err := p.parseList()
		if err != nil {
			return nil, err
		}
		return &inNode{path: path, list: list, negate: negate}, nil
	default:
		return nil, fmt.Errorf("expected an operator after %q, got %q", path, op.text)
	}
}

func (p *exprParser) parseList() ([]exprLiteral, error) {
	var closeKind exprTokKind
	switch {
	case p.accept(tokLParen, ""):
		closeKind = tokRParen
	case p.accept(tokLBrack, ""):
		closeKind = tokRBrack
	default:
		return nil, fmt.Errorf("expected a list after in")
	}
	var out []exprLiteral
	for {
		lit, err := p.parseLiteral()
		if err != nil {
			return nil, err
		}
		out = append(out, lit)
		if p.accept(tokComma, "") {
			continue
		}
		if p.accept(closeKind, "") {
			break
		}
		return nil, fmt.Errorf("expected , or a closing bracket in list")
	}
	return out, nil
}

func (p *exprParser) parseLiteral() (exprLiteral, error) {
	if p.eof() {
		return exprLiteral{}, fmt.Errorf("expected a value")
	}
	t := p.next()
	switch t.kind {
	case tokString:
		return exprLiteral{kind: litString, s: t.text}, nil
	case tokNumber:
		f, _ := strconv.ParseFloat(t.text, 64)
		return exprLiteral{kind: litNumber, n: f, s: t.text}, nil
	case tokIdent:
		switch t.text {
		case "true":
			return exprLiteral{kind: litBool, b: true, s: "true"}, nil
		case "false":
			return exprLiteral{kind: litBool, b: false, s: "false"}, nil
		case "null", "nil":
			return exprLiteral{kind: litNull}, nil
		}
	}
	return exprLiteral{}, fmt.Errorf("expected a value, got %q (strings must be quoted)", t.text)
}

func normalizeOp(op string) string {
	switch op {
	case "=":
		return "=="
	case "<>":
		return "!="
	}
	return op
}

func validExprPath(s string) bool {
	if s == "" || s == "in" || s == "not" || s == "not_in" || s == "true" || s == "false" || s == "null" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return false
		}
		for i, r := range seg {
			ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
			if !ok {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// evaluation

type exprNode interface{ eval(map[string]any) bool }

type andNode struct{ l, r exprNode }
type orNode struct{ l, r exprNode }

func (n *andNode) eval(r map[string]any) bool { return n.l.eval(r) && n.r.eval(r) }
func (n *orNode) eval(r map[string]any) bool  { return n.l.eval(r) || n.r.eval(r) }

type litKind int

const (
	litString litKind = iota
	litNumber
	litBool
	litNull
)

type exprLiteral struct {
	kind litKind
	s    string
	n    float64
	b    bool
}

type cmpNode struct {
	path string
	op   string
	lit  exprLiteral
}

type inNode struct {
	path   string
	list   []exprLiteral
	negate bool
}

func (n *inNode) eval(r map[string]any) bool {
	v := lookupExprPath(r, n.path)
	for _, lit := range n.list {
		if literalEquals(v, lit) {
			return !n.negate
		}
	}
	return n.negate
}

func (n *cmpNode) eval(r map[string]any) bool {
	v := lookupExprPath(r, n.path)
	switch n.op {
	case "==":
		return literalEquals(v, n.lit)
	case "!=":
		return !literalEquals(v, n.lit)
	}
	// Ordering operators.
	switch n.lit.kind {
	case litNumber:
		f, ok := toFloat(v)
		if !ok {
			return false
		}
		return orderCmp(n.op, f, n.lit.n)
	case litString:
		return orderCmpStr(n.op, valueString(v), n.lit.s)
	default:
		return false
	}
}

func orderCmp(op string, a, b float64) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func orderCmpStr(op string, a, b string) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func literalEquals(v any, lit exprLiteral) bool {
	switch lit.kind {
	case litNull:
		return isBlank(v)
	case litBool:
		b, ok := toBool(v)
		return ok && b == lit.b
	case litNumber:
		if f, ok := toFloat(v); ok {
			return f == lit.n
		}
		return valueString(v) == lit.s
	default:
		return valueString(v) == lit.s
	}
}

func lookupExprPath(record map[string]any, path string) any {
	if record == nil {
		return nil
	}
	var cur any = record
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	return cur
}

func isBlank(v any) bool {
	if v == nil {
		return true
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) == ""
	case *string:
		return t == nil || strings.TrimSpace(*t) == ""
	}
	return false
}

func valueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case *string:
		if t == nil {
			return ""
		}
		return strings.TrimSpace(*t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case map[string]any:
		// A resolved relation sibling {value,label}: compare by its value.
		if val, ok := t["value"]; ok {
			return valueString(val)
		}
		return ""
	default:
		if s, ok := v.(fmt.Stringer); ok {
			return strings.TrimSpace(s.String())
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case fmt.Stringer:
		f, err := strconv.ParseFloat(strings.TrimSpace(t.String()), 64)
		return f, err == nil
	}
	return 0, false
}

func toBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no", "":
			return false, true
		}
	case float64:
		return t != 0, true
	case int:
		return t != 0, true
	case int64:
		return t != 0, true
	case nil:
		return false, true
	}
	return false, false
}
