package tool

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

type fakeTool struct {
	addonKey, id string
}

func (f *fakeTool) ID() string            { return f.id }
func (f *fakeTool) AddonKey() string      { return f.addonKey }
func (f *fakeTool) Def() manifest.ToolDef { return manifest.ToolDef{ID: f.id} }
func (f *fakeTool) Execute(_ context.Context, _ map[string]any) (Result, error) {
	return Result{Success: true}, nil
}

func newFake(addon, id string) Tool { return &fakeTool{addonKey: addon, id: id} }

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newFake("stripe", "capture")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(newFake("stripe", "capture")); err == nil {
		t.Fatalf("expected duplicate to error")
	}
	if got, ok := r.ByID("stripe", "capture"); !ok || got.ID() != "capture" {
		t.Fatalf("ByID missing tool")
	}
	if got := r.ByAddon("stripe"); len(got) != 1 {
		t.Fatalf("ByAddon len = %d, want 1", len(got))
	}
	if got := r.All(); len(got) != 1 {
		t.Fatalf("All len = %d, want 1", len(got))
	}
	if got := r.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestRegistry_Replace(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("stripe", "capture"))
	r.Replace(newFake("stripe", "capture"))
	if r.Len() != 1 {
		t.Fatalf("Replace should keep count at 1, got %d", r.Len())
	}
}

func TestRegistry_UnregisterAddon(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("stripe", "capture"))
	_ = r.Register(newFake("stripe", "refund"))
	_ = r.Register(newFake("gcal", "create_event"))

	if n := r.UnregisterAddon("stripe"); n != 2 {
		t.Fatalf("UnregisterAddon removed %d, want 2", n)
	}
	if r.Len() != 1 {
		t.Fatalf("after drain Len = %d, want 1", r.Len())
	}
}

func TestValidate_Required(t *testing.T) {
	schema := []manifest.ToolInputParam{
		{Name: "name", Type: "string", Required: true},
	}
	_, errs := Validate(schema, map[string]any{})
	if len(errs) != 1 || errs[0].Param != "name" {
		t.Fatalf("expected required error on name, got %+v", errs)
	}
}

func TestValidate_NormalizeAndCoerce(t *testing.T) {
	schema := []manifest.ToolInputParam{
		{Name: "order", Type: "string", Normalize: "uppercase"},
		{Name: "qty", Type: "number"},
		{Name: "flag", Type: "boolean"},
	}
	out, errs := Validate(schema, map[string]any{
		"order": "abc-123",
		"qty":   "42",
		"flag":  "true",
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if out["order"] != "ABC-123" {
		t.Errorf("order = %v, want ABC-123", out["order"])
	}
	if out["qty"] != int64(42) {
		t.Errorf("qty = %v (%T), want int64(42)", out["qty"], out["qty"])
	}
	if out["flag"] != true {
		t.Errorf("flag = %v, want true", out["flag"])
	}
}

func TestValidate_DefaultApplied(t *testing.T) {
	schema := []manifest.ToolInputParam{
		{Name: "locale", Type: "string", DefaultValue: "es-MX"},
	}
	out, errs := Validate(schema, map[string]any{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if out["locale"] != "es-MX" {
		t.Errorf("default not applied: got %v", out["locale"])
	}
}

func TestValidate_RegexReject(t *testing.T) {
	schema := []manifest.ToolInputParam{
		{Name: "sku", Type: "string", Validation: "^[A-Z]{3}-[0-9]{4}$"},
	}
	_, errs := Validate(schema, map[string]any{"sku": "abc-000"})
	if len(errs) == 0 {
		t.Fatalf("expected pattern mismatch error")
	}
}

func TestValidate_TypeEmail(t *testing.T) {
	schema := []manifest.ToolInputParam{
		{Name: "to", Type: "email", Required: true},
	}
	if _, errs := Validate(schema, map[string]any{"to": "foo"}); len(errs) == 0 {
		t.Fatalf("expected invalid email error")
	}
	if _, errs := Validate(schema, map[string]any{"to": "a@b.co"}); len(errs) != 0 {
		t.Fatalf("valid email rejected: %+v", errs)
	}
}

func TestResolveEndpoint(t *testing.T) {
	cases := []struct {
		base, endpoint, want string
		wantErr              bool
	}{
		{"", "https://x.test/hook", "https://x.test/hook", false},
		{"https://host.test", "/webhooks/go", "https://host.test/webhooks/go", false},
		{"https://host.test/", "webhooks/go", "https://host.test/webhooks/go", false},
		{"", "/relative", "", true},
		{"https://host.test", "", "", true},
	}
	for _, c := range cases {
		got, err := resolveEndpoint(c.base, c.endpoint)
		if (err != nil) != c.wantErr {
			t.Errorf("resolveEndpoint(%q,%q) err=%v wantErr=%v", c.base, c.endpoint, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("resolveEndpoint(%q,%q) = %q, want %q", c.base, c.endpoint, got, c.want)
		}
	}
}
