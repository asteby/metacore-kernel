package v3

import (
	"strings"
	"testing"
)

func TestRecordExpr_Empty(t *testing.T) {
	e, err := ParseRecordExpr("   ")
	if err != nil {
		t.Fatalf("empty expr must parse: %v", err)
	}
	if !e.IsEmpty() || !e.Eval(nil) || !e.Eval(map[string]any{"status": "draft"}) {
		t.Fatal("empty expr must be always-true")
	}
	var nilExpr *RecordExpr
	if !nilExpr.Eval(map[string]any{}) {
		t.Fatal("nil expr must be always-true")
	}
}

func TestRecordExpr_Eval(t *testing.T) {
	rec := map[string]any{
		"status":   "sent",
		"total":    float64(150),
		"paid":     true,
		"notes":    "",
		"due_on":   "2026-09-15",
		"customer": map[string]any{"value": "c-1", "label": "ACME"},
		"meta":     map[string]any{"tier": "gold"},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"status != 'draft'", true},
		{"status == 'sent'", true},
		{"status = \"sent\"", true},
		{"status <> 'sent'", false},
		{"status in ('sent','accepted')", true},
		{"status in ['draft']", false},
		{"status not_in ('draft','void')", true},
		{"status not in ('sent')", false},
		{"total > 100", true},
		{"total >= 150", true},
		{"total < 100", false},
		{"total == 150", true},
		{"total == 150.0", true},
		{"paid == true", true},
		{"paid != false", true},
		{"notes == null", true},
		{"notes != null", false},
		{"missing == null", true},
		{"missing != 'x'", true},
		{"due_on >= '2026-09-01'", true},
		{"due_on < '2026-09-01'", false},
		{"customer == 'c-1'", true},
		{"meta.tier == 'gold'", true},
		{"status == 'sent' && total > 100", true},
		{"status == 'draft' || total > 100", true},
		{"status == 'draft' || total > 1000", false},
		{"(status == 'draft' || status == 'sent') && paid == true", true},
		{"status == 'draft' || status == 'sent' && paid == false", false},
	}
	for _, c := range cases {
		e, err := ParseRecordExpr(c.expr)
		if err != nil {
			t.Fatalf("%q: parse error: %v", c.expr, err)
		}
		if got := e.Eval(rec); got != c.want {
			t.Errorf("%q: got %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestRecordExpr_Fields(t *testing.T) {
	e, err := ParseRecordExpr("status != 'draft' && (total > 0 || meta.tier == 'gold')")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(e.Fields(), ",")
	if got != "meta,status,total" {
		t.Fatalf("fields = %q", got)
	}
}

func TestRecordExpr_ParseErrors(t *testing.T) {
	bad := []string{
		"status",
		"status ==",
		"status == draft",
		"status === 'x'",
		"status == 'unterminated",
		"status & total",
		"status in 'x'",
		"status in ('a',",
		"(status == 'a'",
		"status == 'a' extra",
		"1 == 1",
		"status == 'a' && ",
		"in == 'x'",
	}
	for _, s := range bad {
		if _, err := ParseRecordExpr(s); err == nil {
			t.Errorf("%q: expected a parse error", s)
		}
	}
}
