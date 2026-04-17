package query

import "testing"

func TestIsSafeIdent(t *testing.T) {
	good := []string{"id", "created_at", "Col1", "_private", "a1b2c3"}
	bad := []string{"", "1col", "col;drop", "col--", "col space", "col.name", "col,other", "col)"}

	for _, s := range good {
		if !isSafeIdent(s) {
			t.Errorf("isSafeIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isSafeIdent(s) {
			t.Errorf("isSafeIdent(%q) = true, want false", s)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"plain":  "plain",
		"50%":    `50\%`,
		"a_b":    `a\_b`,
		`back\s`: `back\\s`,
		`50%_\`:  `50\%\_\\`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterOpConstants(t *testing.T) {
	// Guard against accidental rename of wire-protocol constants.
	wire := map[FilterOp]string{
		OpEq:    "eq",
		OpIlike: "ilike",
		OpIn:    "in",
		OpGte:   "gte",
		OpLte:   "lte",
		OpRange: "range",
	}
	for op, want := range wire {
		if string(op) != want {
			t.Errorf("FilterOp %q = %q, want %q", want, string(op), want)
		}
	}
}
