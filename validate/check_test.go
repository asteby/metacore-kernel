package validate

import (
	"testing"
)

func TestParseRuleString_LaravelPipe(t *testing.T) {
	s := ParseRuleString("required|min:2|max:10|email")
	if !s.Required {
		t.Fatal("expected required")
	}
	if s.Min == nil || *s.Min != 2 {
		t.Fatalf("min=%v", s.Min)
	}
	if s.Max == nil || *s.Max != 10 {
		t.Fatalf("max=%v", s.Max)
	}
	if s.Custom != "email" {
		t.Fatalf("custom=%q", s.Custom)
	}
}

func TestParseRuleString_GoPlayground(t *testing.T) {
	s := ParseRuleString("required,min=2,max=100")
	if !s.Required || s.Min == nil || *s.Min != 2 || s.Max == nil || *s.Max != 100 {
		t.Fatalf("got %+v", s)
	}
}

func TestParseRuleString_CustomSlug(t *testing.T) {
	s := ParseRuleString("$org.tax_id_validator")
	if s.Custom != "$org.tax_id_validator" {
		t.Fatalf("custom=%q", s.Custom)
	}
}

func TestParseRuleString_RegexSlashes(t *testing.T) {
	s := ParseRuleString(`regex:/^[A-Z]{3}$/`)
	if s.Regex != `^[A-Z]{3}$` {
		t.Fatalf("regex=%q", s.Regex)
	}
}

func TestParseRuleString_In(t *testing.T) {
	s := ParseRuleString("in:active,inactive")
	if len(s.Options) != 2 || s.Options[0] != "active" {
		t.Fatalf("options=%v", s.Options)
	}
}

func TestCheck_Required(t *testing.T) {
	got := Check("", Spec{Required: true})
	if len(got) != 1 || got[0].Code != CodeRequired {
		t.Fatalf("got %#v", got)
	}
	if Check("x", Spec{Required: true}) != nil {
		t.Fatal("present value should pass")
	}
}

func TestCheck_MinMaxLength(t *testing.T) {
	min := 3.0
	max := 5.0
	got := Check("ab", Spec{Type: "string", Min: &min, Max: &max})
	if len(got) != 1 || got[0].Code != CodeMin || got[0].Params["kind"] != KindLength {
		t.Fatalf("got %#v", got)
	}
	got = Check("abcdef", Spec{Type: "string", Min: &min, Max: &max})
	if len(got) != 1 || got[0].Code != CodeMax {
		t.Fatalf("got %#v", got)
	}
	if Check("abcd", Spec{Type: "string", Min: &min, Max: &max}) != nil {
		t.Fatal("in-range should pass")
	}
}

func TestCheck_MinMaxNumeric(t *testing.T) {
	min := 1.0
	max := 10.0
	got := Check(0, Spec{Type: "int", Min: &min, Max: &max})
	if len(got) != 1 || got[0].Code != CodeMin || got[0].Params["kind"] != KindValue {
		t.Fatalf("got %#v", got)
	}
}

func TestCheck_RegexAndEmail(t *testing.T) {
	got := Check("nope", Spec{Type: "string", Regex: `^[A-Z]+$`})
	if len(got) != 1 || got[0].Code != CodeRegex {
		t.Fatalf("got %#v", got)
	}
	got = Check("not-an-email", Spec{Custom: "email"})
	if len(got) != 1 || got[0].Code != CodeEmail {
		t.Fatalf("got %#v", got)
	}
	if Check("a@b.com", Spec{Custom: "email"}) != nil {
		t.Fatal("valid email should pass")
	}
}

func TestCheck_EmptySkipsOtherRules(t *testing.T) {
	min := 3.0
	if Check("", Spec{Type: "string", Min: &min, Custom: "email"}) != nil {
		t.Fatal("optional empty should not fire min/email")
	}
}

func TestCheck_CollectsAll(t *testing.T) {
	min := 10.0
	got := Check("ab", Spec{Type: "string", Min: &min, Regex: `^[A-Z]+$`})
	if len(got) != 2 {
		t.Fatalf("want 2 issues, got %#v", got)
	}
}

func TestRegisterCustom(t *testing.T) {
	Register("test.always_fail", func(any) (string, map[string]any) {
		return "custom", map[string]any{"reason": "nope"}
	})
	got := Check("x", Spec{Custom: "test.always_fail"})
	if len(got) != 1 || got[0].Code != "custom" {
		t.Fatalf("got %#v", got)
	}
}
