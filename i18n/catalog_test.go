package i18n

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func langCtx(lang string) context.Context { return WithLanguage(context.Background(), lang) }

func TestLanguageChain(t *testing.T) {
	cases := []struct {
		lang string
		fb   []string
		want []string
	}{
		{"es-MX", []string{"en"}, []string{"es-mx", "es", "en"}},
		{"es_MX", []string{"es", "en"}, []string{"es-mx", "es", "en"}},
		{"zh-Hant-TW", nil, []string{"zh-hant-tw", "zh-hant", "zh"}},
		{"", []string{"pt-BR"}, []string{"pt-br", "pt"}},
		{"en", []string{"en"}, []string{"en"}},
	}
	for _, c := range cases {
		if got := LanguageChain(c.lang, c.fb...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("LanguageChain(%q, %v) = %v, want %v", c.lang, c.fb, got, c.want)
		}
	}
}

func TestCatalogFallbackChain(t *testing.T) {
	c := NewCatalog(map[string]map[string]string{
		"es":    {"title": "Sucursales", "only_es": "solo es"},
		"es-MX": {"title": "Sucursales MX"},
		"en":    {"title": "Branches", "only_en": "only en"},
	})
	cases := map[string][2]string{
		"es-MX/title":   {"es-MX", "title"},
		"es-AR/title":   {"es-AR", "title"},
		"en-US/title":   {"en-US", "title"},
		"fr/title":      {"fr", "title"},
		"es-MX/only_es": {"es-MX", "only_es"},
		"es-MX/only_en": {"es-MX", "only_en"},
		"es-MX/missing": {"es-MX", "missing"},
		"none/title":    {"", "title"},
	}
	want := map[string]string{
		"es-MX/title":   "Sucursales MX",
		"es-AR/title":   "Sucursales",
		"en-US/title":   "Branches",
		"fr/title":      "Branches", // default fallback "en"
		"es-MX/only_es": "solo es",
		"es-MX/only_en": "only en",
		"es-MX/missing": "missing", // contract: key back
		"none/title":    "Branches",
	}
	for name, in := range cases {
		if got := c.Translate(langCtx(in[0]), in[1]); got != want[name] {
			t.Errorf("%s: got %q want %q", name, got, want[name])
		}
	}

	c.WithFallback("es")
	if got := c.Translate(langCtx("fr"), "title"); got != "Sucursales" {
		t.Errorf("fallback es: got %q", got)
	}
	c.WithFallback()
	if got := c.Translate(langCtx("fr"), "title"); got != "title" {
		t.Errorf("no fallback: got %q", got)
	}
	if got := c.Languages(); !reflect.DeepEqual(got, []string{"en", "es", "es-mx"}) {
		t.Errorf("Languages = %v", got)
	}
	if c.Len() != 5 {
		t.Errorf("Len = %d", c.Len())
	}
}

func TestCatalogAddOverridesAndCopies(t *testing.T) {
	src := map[string]map[string]string{"en": {"a": "A"}}
	c := NewCatalog(src)
	src["en"]["a"] = "mutated"
	if got := c.Translate(langCtx("en"), "a"); got != "A" {
		t.Fatalf("NewCatalog must copy its input, got %q", got)
	}
	c.Add("EN", map[string]string{"a": "A2", "": "ignored"})
	if got := c.Translate(langCtx("en"), "a"); got != "A2" {
		t.Fatalf("Add must override, got %q", got)
	}
	m := c.Messages()
	m["en"]["a"] = "x"
	if got := c.Translate(langCtx("en"), "a"); got != "A2" {
		t.Fatalf("Messages must return a copy, got %q", got)
	}
}

func TestInterpolate(t *testing.T) {
	c := NewCatalog(map[string]map[string]string{"es": {"hi": "Hola {name}, tienes {count} {unknown}"}})
	ctx := langCtx("es")
	if got := c.Translate(ctx, "hi", map[string]any{"name": "Ana", "count": 3}); got != "Hola Ana, tienes 3 {unknown}" {
		t.Errorf("map args: %q", got)
	}
	if got := c.Translate(ctx, "hi", "name", "Luis", "count", 1); got != "Hola Luis, tienes 1 {unknown}" {
		t.Errorf("pair args: %q", got)
	}
	if got := c.Translate(ctx, "hi", map[string]string{"name": "Eva"}); got != "Hola Eva, tienes {count} {unknown}" {
		t.Errorf("string map args: %q", got)
	}
	if got := c.Translate(ctx, "hi"); got != "Hola {name}, tienes {count} {unknown}" {
		t.Errorf("no args: %q", got)
	}
	if got := Interpolate("{a}{b", "a", "x", "b"); got != "x{b" {
		t.Errorf("odd args / unclosed brace: %q", got)
	}
}

func TestCatalogConcurrent(t *testing.T) {
	c := NewCatalog(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.Add("es", map[string]string{"k": "v"}) }()
		go func() { defer wg.Done(); _ = c.Translate(langCtx("es"), "k") }()
	}
	wg.Wait()
}

func TestComposeFirstHitWins(t *testing.T) {
	app := NewCatalog(map[string]map[string]string{"es": {"title": "App"}})
	host := NewCatalog(map[string]map[string]string{"es": {"title": "Host", "other": "Otro"}})
	empty := TranslatorFunc(func(context.Context, string, ...any) string { return "" })
	ctx := langCtx("es")

	tr := Compose(nil, empty, app, host)
	if got := tr.Translate(ctx, "title"); got != "App" {
		t.Errorf("first layer must win, got %q", got)
	}
	if got := tr.Translate(ctx, "other"); got != "Otro" {
		t.Errorf("miss must fall through, got %q", got)
	}
	if got := tr.Translate(ctx, "nope"); got != "nope" {
		t.Errorf("all miss must return key, got %q", got)
	}
	if got := Compose(host, app).Translate(ctx, "title"); got != "Host" {
		t.Errorf("order must matter, got %q", got)
	}
	if got := Compose().Translate(ctx, "k"); got != "k" {
		t.Errorf("empty compose: %q", got)
	}
}

func TestHumanize(t *testing.T) {
	cases := []struct{ key, lang, want string }{
		{"models.branches.modal.fields.postal_code", "es-MX", "Código postal"},
		{"models.branches.modal.fields.postal_code", "en", "Postal code"},
		{"models.branches.table.columns.created_at", "es", "Fecha de creación"},
		{"models.branches.table.columns.created_at", "", "Created at"},
		{"models.branches.table.title", "es", "Sucursales"},
		{"models.pickup_points.table.title", "en", "Pickup points"},
		{"models.pickup_points.modal.create_title", "es", "Pickup points"},
		{"models.orders.table.columns.customer_id", "es", "Cliente"},
		{"models.orders.table.columns.customer_id", "en", "Customer"},
		{"models.orders.modal.fields.is_active", "es", "Activo"},
		{"models.orders.modal.placeholders.tracking_url", "en", "Tracking URL"},
		{"models.orders.actions.approve.fields.note", "es", "Nota"},
		{"models.store.modal.fields.rfc", "fr", "RFC"},
		{"CreatedAt", "en", "Created at"},
		{"HTTPServerName", "en", "Http server name"},
		{"foo.bar_baz", "de", "Bar baz"},
	}
	for _, c := range cases {
		if got := Humanize(c.key, c.lang); got != c.want {
			t.Errorf("Humanize(%q, %q) = %q, want %q", c.key, c.lang, got, c.want)
		}
	}
}

func TestHumanizeMissing(t *testing.T) {
	base := NewCatalog(map[string]map[string]string{"es": {"models.b.modal.fields.name": "Nombre propio"}})
	tr := HumanizeMissing(base)
	ctx := langCtx("es")
	if got := tr.Translate(ctx, "models.b.modal.fields.name"); got != "Nombre propio" {
		t.Errorf("hit must win: %q", got)
	}
	if got := tr.Translate(ctx, "models.b.modal.fields.phone"); got != "Teléfono" {
		t.Errorf("miss must humanize: %q", got)
	}
	if got := tr.Translate(ctx, "Already a label"); got != "Already a label" {
		t.Errorf("labels with spaces pass through: %q", got)
	}
	if got := HumanizeMissing(nil).Translate(langCtx("en"), "models.b.table.columns.sort"); got != "Sort" {
		t.Errorf("nil base: %q", got)
	}
}

func TestScopeModelMessages(t *testing.T) {
	in := map[string]map[string]string{"es": {
		"table.title":              "Sucursales",
		"models.other.table.title": "Otro",
		"":                         "ignored",
		"modal.fields.name":        "Nombre",
	}}
	got := ScopeModelMessages("", "branches", in)
	want := map[string]map[string]string{"es": {
		"models.branches.table.title":       "Sucursales",
		"models.other.table.title":          "Otro",
		"models.branches.modal.fields.name": "Nombre",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, ok := in["es"]["models.branches.table.title"]; ok {
		t.Fatal("input must not be modified")
	}
	if got := ScopeModelMessages("app.", "x", map[string]map[string]string{"en": {"t": "T"}}); got["en"]["app.x.t"] != "T" {
		t.Fatalf("custom prefix: %v", got)
	}
}

func TestLoadCatalogFS(t *testing.T) {
	fsys := fstest.MapFS{
		"locales/es.json":          {Data: []byte(`{"models":{"branches":{"table":{"title":"Sucursales"}}},"flat.key":"Plano","list":["a","b"],"n":3}`)},
		"locales/es-MX.yaml":       {Data: []byte("models:\n  branches:\n    table:\n      title: Sucursales MX\n")},
		"locales/en.yml":           {Data: []byte("models.branches.table.title: Branches\n")},
		"locales/branches.fr.json": {Data: []byte(`{"models.branches.table.title":"Succursales"}`)},
		"locales/messages.json":    {Data: []byte(`{"pt":{"hello":"Olá"},"es":{"hello":"Hola"}}`)},
		"locales/README.md":        {Data: []byte("not a catalog")},
		"bad/broken.json":          {Data: []byte(`{`)},
		"bad/multi-wrong.json":     {Data: []byte(`{"title":"not nested"}`)},
		"bad/array.json":           {Data: []byte(`[1,2]`)},
		"empty/es.yaml":            {Data: []byte(``)},
	}
	cat, err := LoadCatalogFS(fsys, "locales/*")
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct{ lang, key, want string }{
		{"es-MX", "models.branches.table.title", "Sucursales MX"},
		{"es-AR", "models.branches.table.title", "Sucursales"},
		{"en", "models.branches.table.title", "Branches"},
		{"fr", "models.branches.table.title", "Succursales"},
		{"es", "flat.key", "Plano"},
		{"es", "list.1", "b"},
		{"es", "n", "3"},
		{"pt-BR", "hello", "Olá"},
		{"es", "hello", "Hola"},
	}
	for _, c := range checks {
		if got := cat.Translate(langCtx(c.lang), c.key); got != c.want {
			t.Errorf("%s %s: got %q want %q", c.lang, c.key, got, c.want)
		}
	}

	if _, err := LoadCatalogFS(fsys, "nothing/*.json"); err == nil || !strings.Contains(err.Error(), "no .json") {
		t.Errorf("no match must fail: %v", err)
	}
	if _, err := LoadCatalogFS(fsys, "[", "locales/*"); err == nil {
		t.Error("bad pattern must fail")
	}
	for _, f := range []string{"bad/broken.json", "bad/multi-wrong.json", "bad/array.json"} {
		if _, err := LoadCatalogFS(fsys, f); err == nil {
			t.Errorf("%s must fail", f)
		}
	}
	if c, err := LoadCatalogFS(fsys, "empty/*.yaml"); err != nil || c.Len() != 0 {
		t.Errorf("empty file: %v %v", c, err)
	}
	if _, err := LoadCatalogFS(nil); err == nil {
		t.Error("nil fs must fail")
	}
}
