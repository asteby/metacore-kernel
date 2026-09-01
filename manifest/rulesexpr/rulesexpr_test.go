package rulesexpr

import "testing"

func TestParseAndEval(t *testing.T) {
	r, err := Parse("amount_paid + amount_due != total")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := r.Eval(map[string]any{"amount_paid": 40, "amount_due": 40, "total": 100})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !got {
		t.Fatalf("expected mismatch to flag true")
	}
	got, err = r.Eval(map[string]any{"amount_paid": 60, "amount_due": 40, "total": 100})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got {
		t.Fatalf("expected balanced totals not to flag")
	}
}

func TestParseFields(t *testing.T) {
	r, err := Parse("qty_on_hand < reorder_point")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fields := r.Fields()
	if len(fields) != 2 || fields[0] != "qty_on_hand" || fields[1] != "reorder_point" {
		t.Fatalf("unexpected fields: %v", fields)
	}
}

func TestParseRejectsInjection(t *testing.T) {
	cases := []string{
		"",
		"total",                       // no operator
		"a == b == c",                 // chained
		"amount; DROP TABLE x == 1",   // illegal char
		"amount == 'x'",               // strings not allowed (arithmetic only)
	}
	for _, c := range cases {
		if _, err := Parse(c); err == nil {
			t.Errorf("Parse(%q): expected error, got nil", c)
		}
	}
}

func TestOperators(t *testing.T) {
	cases := []struct {
		expr string
		env  map[string]any
		want bool
	}{
		{"a >= b", map[string]any{"a": 5, "b": 5}, true},
		{"a > b", map[string]any{"a": 5, "b": 5}, false},
		{"a <= b", map[string]any{"a": 4, "b": 5}, true},
		{"a < b", map[string]any{"a": 6, "b": 5}, false},
		{"a == b", map[string]any{"a": 5, "b": 5}, true},
	}
	for _, c := range cases {
		r, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.expr, err)
		}
		got, err := r.Eval(c.env)
		if err != nil {
			t.Fatalf("Eval(%q): %v", c.expr, err)
		}
		if got != c.want {
			t.Errorf("%q with %v = %v, want %v", c.expr, c.env, got, c.want)
		}
	}
}
