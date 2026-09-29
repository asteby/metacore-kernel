package host

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/i18n"
	"github.com/asteby/metacore-kernel/modelbase"
)

// i18nBranch is the persisted shape; i18nBranchModel declares "models.*"
// keys in its metadata and ships its own catalog with model-relative keys.
type i18nBranch struct {
	modelbase.BaseUUIDModel
	Name       string `json:"name"`
	PostalCode string `json:"postal_code"`
}

func (i18nBranch) TableName() string { return "i18n_branches" }

// i18nKey builds the metadata key of the model registered under model.
func i18nKey(model, rest string) string { return "models." + model + "." + rest }

type i18nBranchModel struct {
	i18nBranch
	key string
}

func (m i18nBranchModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title:             i18nKey(m.key, "table.title"),
		SearchPlaceholder: i18nKey(m.key, "table.search_placeholder"),
		Columns: []modelbase.ColumnDef{
			{Key: "name", Label: i18nKey(m.key, "table.columns.name")},
			{Key: "postal_code", Label: i18nKey(m.key, "table.columns.postal_code")},
			{Key: "literal", Label: "Literal label"},
		},
	}
}

func (m i18nBranchModel) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{
		Title: i18nKey(m.key, "modal.title"),
		Fields: []modelbase.FieldDef{
			{Key: "name", Label: i18nKey(m.key, "modal.fields.name"), Placeholder: i18nKey(m.key, "modal.placeholders.name")},
		},
	}
}

func (m i18nBranchModel) DefineTranslations() map[string]map[string]string {
	return map[string]map[string]string{
		"es": {
			"table.title":              "Sucursales",
			"table.columns.name":       "Nombre",
			"modal.title":              "Sucursal",
			"modal.fields.name":        "Nombre",
			"modal.placeholders.name":  "7 Leguas Centro",
			"table.search_placeholder": "Buscar sucursal…",
		},
		"es-MX": {"table.title": "Sucursales (MX)"},
		"en": {
			"table.title":        "Branches",
			"table.columns.name": "Name",
			"modal.title":        "Branch",
			"modal.fields.name":  "Name",
		},
	}
}

// plainModel has the same keys but no catalog.
type plainModel struct{ i18nBranchModel }

func (plainModel) DefineTranslations() map[string]map[string]string { return nil }

func newI18nApp(t *testing.T, cfg AppConfig, register func(*App)) *fiber.App {
	t.Helper()
	cfg.DB = setupHostTestDB(t)
	cfg.JWTSecret = hostSecret
	app := NewApp(cfg)
	f := fiber.New()
	app.Mount(f.Group("/api"))
	// Registered after Mount on purpose: catalogs must still apply.
	register(app)
	return f
}

type metaResp struct {
	Success bool `json:"success"`
	Data    struct {
		Title             string `json:"title"`
		SearchPlaceholder string `json:"searchPlaceholder"`
		Columns           []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"columns"`
		Fields []struct {
			Key         string `json:"key"`
			Label       string `json:"label"`
			Placeholder string `json:"placeholder"`
		} `json:"fields"`
	} `json:"data"`
}

func getMeta(t *testing.T, f *fiber.App, kind, model, lang string) metaResp {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/metadata/"+kind+"/"+model, nil)
	req.Header.Set("Authorization", "Bearer "+hostToken(t, uuid.New(), "a@b.c", "owner"))
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	resp, err := f.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out metaResp
	if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET %s/%s: %d %s", kind, model, resp.StatusCode, raw)
	}
	return out
}

func colLabel(m metaResp, key string) string {
	for _, c := range m.Data.Columns {
		if c.Key == key {
			return c.Label
		}
	}
	return "<missing>"
}

func TestApp_ModelTranslationsPerAcceptLanguage(t *testing.T) {
	const model = "i18n_branches_a"
	f := newI18nApp(t, AppConfig{I18nDefaultLanguage: "es"}, func(a *App) {
		a.RegisterModel(model, func() modelbase.ModelDefiner { return &i18nBranchModel{key: model} })
	})

	cases := []struct{ lang, title, name, modal string }{
		{"es-MX,es;q=0.9", "Sucursales (MX)", "Nombre", "Sucursal"}, // exact region
		{"es-AR", "Sucursales", "Nombre", "Sucursal"},               // es-AR → es
		{"en-US", "Branches", "Name", "Branch"},                     // en-US → en
		{"fr", "Sucursales", "Nombre", "Sucursal"},                  // fr → default es
		{"", "Sucursales", "Nombre", "Sucursal"},                    // no header → default es
	}
	for _, c := range cases {
		tm := getMeta(t, f, "table", model, c.lang)
		if tm.Data.Title != c.title || colLabel(tm, "name") != c.name {
			t.Errorf("lang %q: title %q name %q", c.lang, tm.Data.Title, colLabel(tm, "name"))
		}
		if colLabel(tm, "literal") != "Literal label" {
			t.Errorf("lang %q: literal labels must be untouched, got %q", c.lang, colLabel(tm, "literal"))
		}
		mm := getMeta(t, f, "modal", model, c.lang)
		if mm.Data.Title != c.modal {
			t.Errorf("lang %q: modal title %q", c.lang, mm.Data.Title)
		}
	}
	// en has no placeholder: the chain en → es(default) fills it.
	if got := getMeta(t, f, "modal", model, "en").Data.Fields[0].Placeholder; got != "7 Leguas Centro" {
		t.Errorf("en placeholder via fallback chain: %q", got)
	}
	// No humanize flag: a key nobody knows stays raw.
	if got := colLabel(getMeta(t, f, "table", model, "en"), "postal_code"); got != i18nKey(model, "table.columns.postal_code") {
		t.Errorf("without humanize the raw key is served, got %q", got)
	}
}

func TestApp_WithTranslationsAndComposeOrder(t *testing.T) {
	const model = "i18n_branches_b"
	host := i18n.NewCatalog(map[string]map[string]string{
		"es": {i18nKey(model, "table.title"): "Sucursales del host"},
	})
	f := newI18nApp(t, AppConfig{Translator: host}, func(a *App) {
		a.RegisterModel(model, func() modelbase.ModelDefiner { return &i18nBranchModel{key: model} },
			WithTranslations(map[string]map[string]string{
				"es": {"table.columns.name": "Nombre (opción)", i18nKey(model, "modal.title"): "Sucursal (opción)"},
			}))
	})
	tm := getMeta(t, f, "table", model, "es")
	if tm.Data.Title != "Sucursales del host" {
		t.Errorf("AppConfig.Translator must win over model catalogs, got %q", tm.Data.Title)
	}
	if got := colLabel(tm, "name"); got != "Nombre (opción)" {
		t.Errorf("WithTranslations must override DefineTranslations, got %q", got)
	}
	if tm.Data.SearchPlaceholder != "Buscar sucursal…" {
		t.Errorf("model catalog fills host gaps, got %q", tm.Data.SearchPlaceholder)
	}
	if got := getMeta(t, f, "modal", model, "es").Data.Title; got != "Sucursal (opción)" {
		t.Errorf("full keys in WithTranslations: %q", got)
	}
	// Default language is "en" when unset.
	if got := getMeta(t, f, "table", model, "").Data.Title; got != "Branches" {
		t.Errorf("default en: %q", got)
	}
}

func TestApp_HumanizeMissing(t *testing.T) {
	const model = "i18n_branches_c"
	f := newI18nApp(t, AppConfig{I18nHumanizeMissing: true}, func(a *App) {
		a.RegisterModel(model, func() modelbase.ModelDefiner { return &plainModel{i18nBranchModel{key: model}} })
	})
	es := getMeta(t, f, "table", model, "es-MX")
	if got := colLabel(es, "postal_code"); got != "Código postal" {
		t.Errorf("es humanized: %q", got)
	}
	if got := colLabel(es, "name"); got != "Nombre" {
		t.Errorf("es humanized name: %q", got)
	}
	en := getMeta(t, f, "table", model, "en")
	if got := colLabel(en, "postal_code"); got != "Postal code" {
		t.Errorf("en humanized: %q", got)
	}
	if en.Data.Title != "I18n branches c" {
		t.Errorf("en humanized title: %q", en.Data.Title)
	}
}

func TestApp_NoI18nLeavesMetadataUntouched(t *testing.T) {
	const model = "i18n_branches_d"
	f := newI18nApp(t, AppConfig{}, func(a *App) {
		a.RegisterModel(model, func() modelbase.ModelDefiner { return &plainModel{i18nBranchModel{key: model}} })
	})
	if got := getMeta(t, f, "table", model, "es").Data.Title; got != i18nKey(model, "table.title") {
		t.Errorf("no translator: %q", got)
	}
}

func TestApp_AddTranslationsFromFS(t *testing.T) {
	const model = "i18n_branches_e"
	fsys := fstest.MapFS{
		"locales/es.json": {Data: []byte(`{"models":{"` + model + `":{"table":{"title":"Desde JSON"}}}}`)},
	}
	cat, err := i18n.LoadCatalogFS(fsys, "locales/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var app *App
	f := newI18nApp(t, AppConfig{}, func(a *App) {
		app = a
		a.RegisterModel(model, func() modelbase.ModelDefiner { return &plainModel{i18nBranchModel{key: model}} })
	})
	// Warm the cache first: AddTranslations must invalidate it.
	if got := getMeta(t, f, "table", model, "es").Data.Title; got != i18nKey(model, "table.title") {
		t.Fatalf("before: %q", got)
	}
	app.AddTranslations(cat.Messages())
	if got := getMeta(t, f, "table", model, "es").Data.Title; got != "Desde JSON" {
		t.Errorf("after AddTranslations: %q", got)
	}
	if got := app.Translator().Translate(i18n.WithLanguage(t.Context(), "es"), i18nKey(model, "table.title")); got != "Desde JSON" {
		t.Errorf("App.Translator: %q", got)
	}
}
