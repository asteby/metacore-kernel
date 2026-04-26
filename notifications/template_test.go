package notifications

import (
	"strings"
	"testing"
)

// TestRender_PlainVar covers the {{var}} happy path so the migration doesn't
// regress existing single-variable templates.
func TestRender_PlainVar(t *testing.T) {
	got := Render("hello {{name}}", map[string]string{"name": "world"})
	if got != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

// TestRender_MissingVarStripped: an unmapped {{var}} must not leak to the
// customer; it gets stripped along with surrounding whitespace.
func TestRender_MissingVarStripped(t *testing.T) {
	got := Render("hello {{name}}", map[string]string{})
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

// TestRender_SectionTruthy: section body renders when the gating var has a
// non-empty value, with inner vars expanded.
func TestRender_SectionTruthy(t *testing.T) {
	tmpl := "ticket *{{number}}*{{#priority}} — prioridad {{priority}}{{/priority}}: {{title}}."
	vars := map[string]string{
		"number":   "TICK-33",
		"priority": "Urgente",
		"title":    "Consulta SUNAT",
	}
	got := Render(tmpl, vars)
	want := "ticket *TICK-33* — prioridad Urgente: Consulta SUNAT."
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// TestRender_SectionEmpty drops the whole {{#priority}}...{{/priority}} block
// when priority is empty — this is the regression that motivated the package.
func TestRender_SectionEmpty(t *testing.T) {
	tmpl := "ticket *{{number}}*{{#priority}} — prioridad {{priority}}{{/priority}}: {{title}}."
	vars := map[string]string{
		"number":   "SUP-7",
		"priority": "",
		"title":    "Duda simple",
	}
	got := Render(tmpl, vars)
	want := "ticket *SUP-7*: Duda simple."
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "prioridad") {
		t.Fatalf("section should be removed when var empty: %q", got)
	}
}

// TestRender_SectionMissingKey: the var isn't in the map at all → treated as
// empty, section dropped.
func TestRender_SectionMissingKey(t *testing.T) {
	got := Render("a{{#x}}body{{/x}}b", map[string]string{})
	if got != "ab" {
		t.Fatalf("got %q, want %q", got, "ab")
	}
}

// TestRender_SectionFalseString: literal "false" is treated as falsy so
// callers can pass through booleans flattened with strconv.FormatBool.
func TestRender_SectionFalseString(t *testing.T) {
	got := Render("a{{#x}}body{{/x}}b", map[string]string{"x": "false"})
	if got != "ab" {
		t.Fatalf("got %q, want %q", got, "ab")
	}
}

// TestRender_InvertedSection: {{^x}} renders only when x is falsy.
func TestRender_InvertedSection(t *testing.T) {
	tmpl := "{{#x}}has{{/x}}{{^x}}none{{/x}}"
	if got := Render(tmpl, map[string]string{"x": "yes"}); got != "has" {
		t.Fatalf("truthy: got %q", got)
	}
	if got := Render(tmpl, map[string]string{"x": ""}); got != "none" {
		t.Fatalf("empty: got %q", got)
	}
	if got := Render(tmpl, map[string]string{}); got != "none" {
		t.Fatalf("missing: got %q", got)
	}
}

// TestRender_NestedSections covers a section inside a section to lock in the
// scanner's recursive handling.
func TestRender_NestedSections(t *testing.T) {
	tmpl := "{{#outer}}O({{#inner}}I({{name}}){{/inner}}){{/outer}}"
	vars := map[string]string{
		"outer": "1",
		"inner": "1",
		"name":  "x",
	}
	if got := Render(tmpl, vars); got != "O(I(x))" {
		t.Fatalf("nested truthy: got %q", got)
	}

	vars["inner"] = ""
	if got := Render(tmpl, vars); got != "O()" {
		t.Fatalf("inner empty: got %q", got)
	}

	vars["outer"] = ""
	if got := Render(tmpl, vars); got != "" {
		t.Fatalf("outer empty: got %q", got)
	}
}

// TestRender_AdjacentSections covers two sections in a row sharing no overlap;
// the scanner must not get confused about which {{/name}} closes which {{#}}.
func TestRender_AdjacentSections(t *testing.T) {
	tmpl := "{{#a}}A{{/a}}-{{#b}}B{{/b}}"
	got := Render(tmpl, map[string]string{"a": "1", "b": "1"})
	if got != "A-B" {
		t.Fatalf("got %q, want %q", got, "A-B")
	}
	got = Render(tmpl, map[string]string{"a": "1"})
	if got != "A-" {
		t.Fatalf("got %q, want %q", got, "A-")
	}
}

// TestRender_LiteralCarriedThrough verifies normal text outside any tag is
// emitted verbatim — character-for-character, no whitespace squashing inside.
func TestRender_LiteralCarriedThrough(t *testing.T) {
	got := Render("just text, no tags here.", map[string]string{})
	if got != "just text, no tags here." {
		t.Fatalf("got %q", got)
	}
}

// TestRender_EmptyTemplate is a defensive case so callers can pass through
// untrusted input safely.
func TestRender_EmptyTemplate(t *testing.T) {
	if got := Render("", map[string]string{"x": "y"}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestRender_UnclosedSection: documents that a missing {{/name}} leaves the
// open tag visible in the output instead of silently swallowing everything
// after it (decision: surface the bug to the template author).
func TestRender_UnclosedSection(t *testing.T) {
	got := Render("a {{#x}}body never closes", map[string]string{"x": "1"})
	// {{#x}} is left literal, but body still emits because the children were
	// parsed and folded into the top level.
	if !strings.Contains(got, "{{#x}}") {
		t.Fatalf("unclosed open tag should be visible, got %q", got)
	}
	if !strings.Contains(got, "body never closes") {
		t.Fatalf("orphaned body should still render, got %q", got)
	}
}

// TestRender_StrayCloseTag: a {{/x}} without a matching open is left literal,
// not silently dropped.
func TestRender_StrayCloseTag(t *testing.T) {
	got := Render("a {{/x}} b", map[string]string{})
	if !strings.Contains(got, "{{/x}}") {
		t.Fatalf("stray close should stay literal, got %q", got)
	}
}

// TestRender_VoucherTemplate replays the exact production template that
// regressed during the kernel migration so this test failing is the same
// as the host-side smoke test failing.
func TestRender_VoucherTemplate(t *testing.T) {
	tmpl := "Recibí tu comprobante de *{{order_number}}*.{{#detected_amount}} Monto: {{currency_symbol}}{{detected_amount}}.{{/detected_amount}} Lo revisamos y te confirmamos."

	withAmount := Render(tmpl, map[string]string{
		"order_number":    "OR-25",
		"detected_amount": "141.00",
		"currency_symbol": "S/",
	})
	if !strings.Contains(withAmount, "Monto: S/141.00") {
		t.Errorf("amount section missing: %q", withAmount)
	}

	withoutAmount := Render(tmpl, map[string]string{
		"order_number":    "OR-26",
		"detected_amount": "",
	})
	if strings.Contains(withoutAmount, "Monto") {
		t.Errorf("amount section should be omitted when empty: %q", withoutAmount)
	}
	if !strings.Contains(withoutAmount, "OR-26") {
		t.Errorf("order_number must still appear: %q", withoutAmount)
	}
}

// TestRender_TicketStatusChanged is the kernel-side mirror of a host-side
// backend test that was skipped during the kernel migration. Keeping a copy
// here lets the kernel's CI catch regressions independently of the host.
func TestRender_TicketStatusChanged(t *testing.T) {
	tmpl := "Tu ticket *{{ticket_number}}* cambió a *{{status_label}}*{{#priority_label}} — prioridad {{priority_label}}{{/priority_label}}: *{{title}}*."

	withPriority := Render(tmpl, map[string]string{
		"ticket_number":  "TICK-33",
		"status_label":   "En progreso",
		"priority_label": "Urgente",
		"title":          "Consulta sobre SUNAT",
	})
	want := "Tu ticket *TICK-33* cambió a *En progreso* — prioridad Urgente: *Consulta sobre SUNAT*."
	if withPriority != want {
		t.Errorf("with priority: got %q\nwant %q", withPriority, want)
	}

	noPriority := Render(tmpl, map[string]string{
		"ticket_number":  "SUP-7",
		"status_label":   "Resuelto",
		"priority_label": "",
		"title":          "Duda simple",
	})
	wantNo := "Tu ticket *SUP-7* cambió a *Resuelto*: *Duda simple*."
	if noPriority != wantNo {
		t.Errorf("no priority: got %q\nwant %q", noPriority, wantNo)
	}
	if strings.Contains(noPriority, "prioridad") {
		t.Errorf("priority section must vanish, got %q", noPriority)
	}
}

// TestRender_CollapsesBlankLines: when a multiline section disappears it can
// leave behind a run of blank lines.  We collapse 3+ to 2 so the output stays
// readable on chat clients.
func TestRender_CollapsesBlankLines(t *testing.T) {
	tmpl := "line1\n\n{{#x}}\nmiddle\n{{/x}}\n\nline2"
	got := Render(tmpl, map[string]string{"x": ""})
	if strings.Count(got, "\n\n\n") > 0 {
		t.Fatalf("triple newline should be collapsed: %q", got)
	}
}

// TestRender_WhitespaceInTagName: tolerate {{ name }} as well as {{name}} —
// hand-written templates often have stray spaces.
func TestRender_WhitespaceInTagName(t *testing.T) {
	got := Render("hi {{ name }}", map[string]string{"name": "x"})
	if got != "hi x" {
		t.Fatalf("got %q", got)
	}
}
