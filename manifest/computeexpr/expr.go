// Package computeexpr is a SMALL, SAFE numeric expression engine shared by the
// declarative compute engine's runtime (dynamic) and BOTH manifest validators
// (v3 strict + legacy/install strict). It lives in its own leaf package to
// avoid an import cycle: dynamic imports manifest, so the parser cannot live in
// either manifest package if dynamic is to reuse it. Three jobs, one grammar:
//
//	Eval      — evaluates an expression to a float64 against an env map
//	            (Tier-2 formulas, in Go, before the DB write).
//	RenderSQL — re-emits an expression as a SQL fragment with every identifier
//	            double-quoted (Tier-1 rollup expr, embedded into aggregate SQL).
//	Validate  — parses without evaluating, to reject malformed/injection input
//	            at manifest validation time, optionally checking that every
//	            identifier resolves to an allowed column.
//
// GRAMMAR (recursive descent, standard precedence):
//
//	expr   := term  (('+' | '-') term)*
//	term   := factor (('*' | '/') factor)*
//	factor := NUMBER | IDENT | '(' expr ')' | ('-' | '+') factor
//
// The lexer ONLY accepts: decimal numbers, identifiers matching
// [A-Za-z_][A-Za-z0-9_]*, whitespace, the operators + - * / and parentheses.
// Any other byte (quotes, semicolons, comments, %, etc.) is a hard error — no
// function calls, no subscripts, no SQL keywords. This is the security boundary
// for the raw-SQL Tier-1 expr path.

package computeexpr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// IdentRe is the allowlist for a single bare identifier (column name, rollup
// target / from). Anchored, so it matches the WHOLE token.
var IdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// --- token types -----------------------------------------------------------

type tokKind int

const (
	tkNumber tokKind = iota
	tkIdent
	tkPlus
	tkMinus
	tkStar
	tkSlash
	tkLParen
	tkRParen
	tkEOF
)

type token struct {
	kind tokKind
	text string
}

// lexArith tokenizes src under the strict allowlist. Any disallowed byte is an
// error — this is what rejects quotes/semicolons/SQL injection before parsing.
func lexArith(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '+':
			toks = append(toks, token{tkPlus, "+"})
			i++
		case c == '-':
			toks = append(toks, token{tkMinus, "-"})
			i++
		case c == '*':
			toks = append(toks, token{tkStar, "*"})
			i++
		case c == '/':
			toks = append(toks, token{tkSlash, "/"})
			i++
		case c == '(':
			toks = append(toks, token{tkLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tkRParen, ")"})
			i++
		case c >= '0' && c <= '9' || c == '.':
			j := i
			seenDot := false
			for j < len(src) {
				d := src[j]
				if d >= '0' && d <= '9' {
					j++
					continue
				}
				if d == '.' && !seenDot {
					seenDot = true
					j++
					continue
				}
				break
			}
			num := src[i:j]
			if num == "." {
				return nil, fmt.Errorf("invalid number %q", num)
			}
			if _, err := strconv.ParseFloat(num, 64); err != nil {
				return nil, fmt.Errorf("invalid number %q", num)
			}
			toks = append(toks, token{tkNumber, num})
			i = j
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
			j := i
			for j < len(src) {
				d := src[j]
				if d == '_' || (d >= 'A' && d <= 'Z') || (d >= 'a' && d <= 'z') || (d >= '0' && d <= '9') {
					j++
					continue
				}
				break
			}
			toks = append(toks, token{tkIdent, src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("illegal character %q in expression", string(c))
		}
	}
	toks = append(toks, token{tkEOF, ""})
	return toks, nil
}

// --- AST --------------------------------------------------------------------

type node interface{}

type numNode struct{ val float64 }
type identNode struct{ name string }
type unaryNode struct {
	op    byte // '-' or '+'
	child node
}
type binNode struct {
	op          byte // '+','-','*','/'
	left, right node
}

// parser is a single-pass recursive-descent parser over the token slice.
type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func parseArith(src string) (node, []string, error) {
	toks, err := lexArith(src)
	if err != nil {
		return nil, nil, err
	}
	if len(toks) == 1 { // only EOF
		return nil, nil, fmt.Errorf("empty expression")
	}
	p := &parser{toks: toks}
	n, err := p.parseExpr()
	if err != nil {
		return nil, nil, err
	}
	if p.peek().kind != tkEOF {
		return nil, nil, fmt.Errorf("unexpected token %q", p.peek().text)
	}
	idents := collectIdents(n, nil)
	return n, idents, nil
}

func (p *parser) parseExpr() (node, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().kind {
		case tkPlus:
			p.next()
			r, err := p.parseTerm()
			if err != nil {
				return nil, err
			}
			left = &binNode{op: '+', left: left, right: r}
		case tkMinus:
			p.next()
			r, err := p.parseTerm()
			if err != nil {
				return nil, err
			}
			left = &binNode{op: '-', left: left, right: r}
		default:
			return left, nil
		}
	}
}

func (p *parser) parseTerm() (node, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().kind {
		case tkStar:
			p.next()
			r, err := p.parseFactor()
			if err != nil {
				return nil, err
			}
			left = &binNode{op: '*', left: left, right: r}
		case tkSlash:
			p.next()
			r, err := p.parseFactor()
			if err != nil {
				return nil, err
			}
			left = &binNode{op: '/', left: left, right: r}
		default:
			return left, nil
		}
	}
}

func (p *parser) parseFactor() (node, error) {
	t := p.peek()
	switch t.kind {
	case tkNumber:
		p.next()
		v, _ := strconv.ParseFloat(t.text, 64)
		return &numNode{val: v}, nil
	case tkIdent:
		p.next()
		return &identNode{name: t.text}, nil
	case tkLParen:
		p.next()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tkRParen {
			return nil, fmt.Errorf("expected ')'")
		}
		p.next()
		return inner, nil
	case tkMinus:
		p.next()
		child, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: '-', child: child}, nil
	case tkPlus:
		p.next()
		child, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: '+', child: child}, nil
	default:
		return nil, fmt.Errorf("unexpected token %q", t.text)
	}
}

func collectIdents(n node, acc []string) []string {
	switch v := n.(type) {
	case *identNode:
		acc = append(acc, v.name)
	case *unaryNode:
		acc = collectIdents(v.child, acc)
	case *binNode:
		acc = collectIdents(v.left, acc)
		acc = collectIdents(v.right, acc)
	}
	return acc
}

// --- evaluation (Tier-2) ----------------------------------------------------

// evalArith evaluates src against env (identifier -> float64). Missing
// identifiers resolve to 0. Division by zero yields 0 (ERP-safe: a computed
// column doesn't blow up a write).
func Eval(src string, env map[string]float64) (float64, error) {
	n, _, err := parseArith(src)
	if err != nil {
		return 0, err
	}
	return evalNode(n, env), nil
}

func evalNode(n node, env map[string]float64) float64 {
	switch v := n.(type) {
	case *numNode:
		return v.val
	case *identNode:
		return env[v.name] // missing -> zero value 0
	case *unaryNode:
		x := evalNode(v.child, env)
		if v.op == '-' {
			return -x
		}
		return x
	case *binNode:
		l := evalNode(v.left, env)
		r := evalNode(v.right, env)
		switch v.op {
		case '+':
			return l + r
		case '-':
			return l - r
		case '*':
			return l * r
		case '/':
			if r == 0 {
				return 0
			}
			return l / r
		}
	}
	return 0
}

// --- SQL rendering (Tier-1) -------------------------------------------------

// renderArithSQL re-emits src as a SQL arithmetic fragment with every
// identifier double-quoted. Because it round-trips through the same strict
// parser, the output can only contain numbers, double-quoted identifiers, the
// operators + - * / and parentheses — safe to embed in an aggregate.
func RenderSQL(src string) (string, error) {
	n, _, err := parseArith(src)
	if err != nil {
		return "", err
	}
	return renderNode(n), nil
}

func renderNode(n node) string {
	switch v := n.(type) {
	case *numNode:
		return strconv.FormatFloat(v.val, 'f', -1, 64)
	case *identNode:
		return `"` + v.name + `"`
	case *unaryNode:
		return string(v.op) + "(" + renderNode(v.child) + ")"
	case *binNode:
		return "(" + renderNode(v.left) + " " + string(v.op) + " " + renderNode(v.right) + ")"
	}
	return ""
}

// validateArith parses src for syntactic validity and, when allowed is
// non-nil, checks that every identifier is a key of allowed (a real column).
// It does NOT evaluate. Used by both v3 and legacy validators so a manifest
// fails identically on both surfaces.
func Validate(src string, allowed map[string]struct{}) error {
	_, idents, err := parseArith(src)
	if err != nil {
		return err
	}
	if allowed == nil {
		return nil
	}
	var unknown []string
	for _, id := range idents {
		if _, ok := allowed[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("references unknown column(s): %s", strings.Join(unknown, ", "))
	}
	return nil
}

// toFloat coerces an arbitrary value pulled from an input/row map to float64.
// Strings are parsed (forms often arrive as strings); nil / unparseable -> 0.
func ToFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint64:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}
