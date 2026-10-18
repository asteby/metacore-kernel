package dataload

import (
	"strings"
	"testing"
)

func TestParseProfile_RejectsUnknownField(t *testing.T) {
	_, err := ParseProfile([]byte(`{"name":"x","entity":"e","key":["a"],"fields":[{"target":"a","from":["A"]}],"typo":1}`))
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("debía rechazar el campo desconocido, got %v", err)
	}
}

func TestParseProfile_ValidJSON(t *testing.T) {
	p, err := ParseProfile([]byte(`{"name":"x","version":"2","entity":"e","key":["a"],"fields":[{"target":"a","from":["A"],"required":true}]}`))
	if err != nil || p.Version != "2" || len(p.Fields) != 1 {
		t.Fatalf("perfil válido rechazado: %v %+v", err, p)
	}
}

func TestProfileValidate_KeyMustBeAField(t *testing.T) {
	p := catalogProfile()
	p.Key = []string{"sku"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `key "sku"`) {
		t.Fatalf("got %v", err)
	}
}

func TestProfileValidate_RejectsMediaTransformAndUnknownClean(t *testing.T) {
	p := catalogProfile()
	p.Fields[5] = Field{Target: "images", From: []string{"Imágenes"}, Transform: "media_url_list"}
	p.Fields[1].Clean = []string{"inventada"}
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "media:true") || !strings.Contains(err.Error(), "inventada") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveOverrides_RenameDropIgnoreDefaultsAndAddField(t *testing.T) {
	base := catalogProfile()
	ov := &Overrides{
		Org:           "org-a",
		IgnoreHeaders: []string{"Meta: _wcct_*"},
		Rename:        map[string][]string{"name": {"Título"}},
		Drop:          []string{"description"},
		Defaults:      map[string]any{"channel": "web"},
		Fields:        []Field{{Target: "brand", From: []string{"Marca"}}},
	}
	p, err := base.Resolve(ov)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.field("description"); ok {
		t.Fatal("description debía eliminarse")
	}
	if f, _ := p.field("name"); f.From[len(f.From)-1] != "Título" {
		t.Fatalf("rename no aplicado: %v", f.From)
	}
	if _, ok := p.field("brand"); !ok || p.Defaults["channel"] != "web" || !p.headerIgnored("Meta: _wcct_foo") {
		t.Fatal("override incompleto")
	}
}

func TestResolveOverrides_DoesNotMutateBase(t *testing.T) {
	base := catalogProfile()
	h := base.Hash()
	_, _ = base.Resolve(&Overrides{Drop: []string{"price"}, Rename: map[string][]string{"name": {"X"}}})
	if base.Hash() != h || len(base.Fields) != 6 || len(base.Fields[1].From) != 2 {
		t.Fatal("Resolve mutó el perfil base")
	}
}

func TestResolveOverrides_RenameUnknownTargetFails(t *testing.T) {
	if _, err := catalogProfile().Resolve(&Overrides{Rename: map[string][]string{"nada": {"X"}}}); err == nil {
		t.Fatal("debía fallar al renombrar un target inexistente")
	}
}

func TestProfileHash_ChangesWithOverrides(t *testing.T) {
	base := catalogProfile()
	a, _ := base.Resolve(nil)
	b, _ := base.Resolve(&Overrides{Drop: []string{"price"}})
	if a.Hash() == b.Hash() {
		t.Fatal("el hash debía cambiar con los overrides")
	}
	c, _ := base.Resolve(nil)
	if a.Hash() != c.Hash() {
		t.Fatal("el hash debe ser estable")
	}
}

func TestClean_Rules(t *testing.T) {
	cases := []struct {
		rules []string
		in    string
		want  string
	}{
		{[]string{CleanUnescapeNewline}, `a\nb`, "a\nb"},
		{[]string{CleanStripHTML}, "<p>Hola&nbsp;<b>mundo</b></p>", "Hola mundo"},
		{[]string{CleanTrimQuotes}, "“GOD’S”", `GOD'S`},
		{[]string{CleanCollapseSpaces}, "  a   b  ", "a b"},
		{[]string{CleanNullIfEmptyish}, "N/A", ""},
		{[]string{CleanLower, CleanTrim}, "  AbC ", "abc"},
		{[]string{CleanUpper}, "abc", "ABC"},
	}
	for _, c := range cases {
		if got := applyClean(c.rules, c.in); got != c.want {
			t.Errorf("%v(%q) = %q, quería %q", c.rules, c.in, got, c.want)
		}
	}
}

func TestSplitMedia_DedupAndSeparators(t *testing.T) {
	got := splitMedia("http://a/1.jpg, http://a/2.jpg;http://a/1.jpg|http://a/3.jpg\nhttp://a/4.jpg", "")
	if len(got) != 4 {
		t.Fatalf("got %v", got)
	}
	if got := splitMedia("a ## b", "##"); len(got) != 2 {
		t.Fatalf("separador propio: %v", got)
	}
}
