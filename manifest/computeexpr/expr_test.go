package computeexpr

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEval_Arithmetic(t *testing.T) {
	env := map[string]float64{
		"quantity":   3,
		"unit_price": 10,
		"discount":   5,
		"subtotal":   25,
		"tax_amount": 4,
	}
	cases := []struct {
		expr string
		want float64
	}{
		{"1 + 2", 3},
		{"2 * 3 + 4", 10},                        // precedence
		{"2 + 3 * 4", 14},                        // precedence
		{"(2 + 3) * 4", 20},                      // parens
		{"10 / 4", 2.5},                          // decimal result
		{"1.5 + 2.25", 3.75},                     // decimal literals
		{"-5 + 3", -2},                           // unary minus
		{"+5 - 3", 2},                            // unary plus
		{"quantity * unit_price - discount", 25}, // identifiers
		{"subtotal + tax_amount", 29},
		{"missing_col + 1", 1},    // missing identifier = 0
		{"10 / 0", 0},             // div by zero = 0 (ERP-safe)
		{"2 * (3 + (4 - 1))", 12}, // nested parens
	}
	for _, c := range cases {
		got, err := Eval(c.expr, env)
		if err != nil {
			t.Fatalf("Eval(%q) unexpected error: %v", c.expr, err)
		}
		if !approx(got, c.want) {
			t.Errorf("Eval(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestEval_RejectsMalformedAndInjection(t *testing.T) {
	bad := []string{
		``,                 // empty
		`   `,              // whitespace only
		`1 +`,              // trailing operator
		`* 2`,              // leading operator
		`(1 + 2`,           // unbalanced paren
		`1 + 2)`,           // extra close paren
		`1 ; DROP TABLE x`, // semicolon / SQL
		`'abc'`,            // quote
		`"col"`,            // double quote
		// NOTE: `col -- comment` is NOT malformed here — `--` lexes as two
		// unary-minus operators (col - -comment), harmless arithmetic, and a
		// `-- ...` line comment is impossible because whitespace/words are only
		// ever idents or operators (no raw text passthrough). The SQL-comment
		// threat is neutralised by construction, so we don't assert on it.
		`col # comment`, // '#' is an illegal character
		`SUM(x)`,        // function call -> '(' after ident not allowed
		`a.b`,           // subscript / dot
		`a % b`,         // unsupported operator
		`a ^ b`,         // unsupported operator
		`col[0]`,        // subscript
		`1 + 2 = 3`,     // equality
		`0x10`,          // hex literal not allowed (x is ident -> "0" "x10"? lexer splits)
	}
	for _, expr := range bad {
		if _, err := Eval(expr, nil); err == nil {
			t.Errorf("Eval(%q) expected error, got nil", expr)
		}
	}
}

func TestValidate_IdentifierAllowlist(t *testing.T) {
	cols := map[string]struct{}{"quantity": {}, "unit_price": {}, "discount": {}}

	if err := Validate("quantity * unit_price - discount", cols); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	// Unknown column referenced.
	if err := Validate("quantity * price", cols); err == nil {
		t.Errorf("expected unknown-column error, got nil")
	}
	// nil allowed set -> only syntax checked.
	if err := Validate("anything + 1", nil); err != nil {
		t.Errorf("expected syntax-only pass, got %v", err)
	}
	// Injection rejected even with nil allowed set.
	if err := Validate("1; DROP TABLE t", nil); err == nil {
		t.Errorf("expected injection rejection, got nil")
	}
}

func TestRenderSQL_DoubleQuotesIdents(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"subtotal + tax_amount", `("subtotal" + "tax_amount")`},
		{"quantity * unit_price - discount", `(("quantity" * "unit_price") - "discount")`},
		{"2 * (a + 3)", `(2 * ("a" + 3))`},
	}
	for _, c := range cases {
		got, err := RenderSQL(c.expr)
		if err != nil {
			t.Fatalf("RenderSQL(%q) error: %v", c.expr, err)
		}
		if got != c.want {
			t.Errorf("RenderSQL(%q) = %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestRenderSQL_RejectsInjection(t *testing.T) {
	if _, err := RenderSQL("subtotal); DROP TABLE x; --"); err == nil {
		t.Errorf("expected rejection of injection in RenderSQL")
	}
}

func TestToFloat(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{float64(3.5), 3.5},
		{int(4), 4},
		{int64(7), 7},
		{"12.5", 12.5},
		{"  9 ", 9},
		{"notanumber", 0},
		{nil, 0},
		{true, 1},
		{false, 0},
	}
	for _, c := range cases {
		if got := ToFloat(c.in); !approx(got, c.want) {
			t.Errorf("ToFloat(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
