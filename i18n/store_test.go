package i18n

import (
	"context"
	"sort"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return db
}

func TestRegisterAndGetAddonI18n_FlatI18n(t *testing.T) {
	db := openTestDB(t)
	m := manifest.Manifest{
		Key:     "demo",
		Version: "1.0.0",
		I18n: map[string]map[string]string{
			"es": {"demo.title": "Demo", "demo.cta": "Acción"},
			"en": {"demo.title": "Demo", "demo.cta": "Action"},
		},
	}
	if err := RegisterAddonI18n(context.Background(), db, "demo", m); err != nil {
		t.Fatalf("register: %v", err)
	}
	es, err := GetAddonI18n(context.Background(), db, "es")
	if err != nil {
		t.Fatalf("get es: %v", err)
	}
	if es["demo.title"] != "Demo" || es["demo.cta"] != "Acción" {
		t.Fatalf("expected es strings, got %#v", es)
	}
	en, err := GetAddonI18n(context.Background(), db, "en")
	if err != nil {
		t.Fatalf("get en: %v", err)
	}
	if en["demo.cta"] != "Action" {
		t.Fatalf("expected en cta=Action, got %v", en["demo.cta"])
	}
}

func TestRegisterAddonI18n_MetadataI18nUnfolded(t *testing.T) {
	db := openTestDB(t)
	m := manifest.Manifest{
		Key: "tires",
		MetadataI18n: map[string]manifest.MetadataLocale{
			"es": {
				Name:        "Llantas",
				Description: "Gestión de llantas",
				Features:    []string{"Inventario", "Servicios"},
			},
		},
	}
	if err := RegisterAddonI18n(context.Background(), db, "tires", m); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := GetAddonI18nFor(context.Background(), db, "tires", "es")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := map[string]string{
		"catalog.name":        "Llantas",
		"catalog.description": "Gestión de llantas",
		"catalog.features.0":  "Inventario",
		"catalog.features.1":  "Servicios",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: want %q got %q", k, v, got[k])
		}
	}
}

func TestRegisterAddonI18n_UpgradeDropsRemovedKeys(t *testing.T) {
	db := openTestDB(t)
	v1 := manifest.Manifest{
		Key: "demo",
		I18n: map[string]map[string]string{
			"es": {"demo.title": "v1", "demo.legacy": "legacy"},
		},
	}
	if err := RegisterAddonI18n(context.Background(), db, "demo", v1); err != nil {
		t.Fatalf("register v1: %v", err)
	}
	v2 := manifest.Manifest{
		Key: "demo",
		I18n: map[string]map[string]string{
			"es": {"demo.title": "v2"},
		},
	}
	if err := RegisterAddonI18n(context.Background(), db, "demo", v2); err != nil {
		t.Fatalf("register v2: %v", err)
	}
	got, err := GetAddonI18nFor(context.Background(), db, "demo", "es")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["demo.title"] != "v2" {
		t.Fatalf("expected title=v2, got %v", got["demo.title"])
	}
	if _, ok := got["demo.legacy"]; ok {
		t.Fatalf("expected demo.legacy purged on upgrade, got %v", got)
	}
}

func TestUnregisterAddonI18n_RemovesEverything(t *testing.T) {
	db := openTestDB(t)
	m := manifest.Manifest{
		Key:  "demo",
		I18n: map[string]map[string]string{"es": {"a": "1"}, "en": {"a": "1"}},
	}
	if err := RegisterAddonI18n(context.Background(), db, "demo", m); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := UnregisterAddonI18n(context.Background(), db, "demo"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	es, _ := GetAddonI18nFor(context.Background(), db, "demo", "es")
	en, _ := GetAddonI18nFor(context.Background(), db, "demo", "en")
	if len(es) != 0 || len(en) != 0 {
		t.Fatalf("expected empty after unregister, got es=%v en=%v", es, en)
	}
}

func TestGetAddonI18n_MultipleAddonsMerge(t *testing.T) {
	db := openTestDB(t)
	if err := RegisterAddonI18n(context.Background(), db, "a",
		manifest.Manifest{Key: "a", I18n: map[string]map[string]string{"es": {"a.x": "ax"}}}); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := RegisterAddonI18n(context.Background(), db, "b",
		manifest.Manifest{Key: "b", I18n: map[string]map[string]string{"es": {"b.y": "by"}}}); err != nil {
		t.Fatalf("b: %v", err)
	}
	got, err := GetAddonI18n(context.Background(), db, "es")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 || got["a.x"] != "ax" || got["b.y"] != "by" {
		t.Fatalf("expected merged catalog, got %v (keys=%v)", got, keys)
	}
}

func TestRegisterAddonI18n_NilDBIsNoop(t *testing.T) {
	if err := RegisterAddonI18n(context.Background(), nil, "x", manifest.Manifest{}); err != nil {
		t.Fatalf("nil db must be noop, got %v", err)
	}
}
