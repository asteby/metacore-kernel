package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

// packBundle builds a minimal tar.gz with manifest + arbitrary extra files,
// so the locale-extraction path can be exercised against a fixture that owns
// both `manifest.json` and `locales/*.json` entries.
func packBundle(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return &buf
}

const v3ManifestWithLocales = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {
    "key": "demo_i18n",
    "name": "Demo i18n",
    "version": "0.1.0",
    "icon": { "type": "lucide", "slug": "Globe" }
  },
  "compatibility": {
    "requires": [{ "key": "kernel", "version": ">=3.0.0" }]
  },
  "i18n": {
    "default_locale": "es-MX",
    "bundles": [
      { "locale": "es-MX", "path": "locales/es-MX.json" },
      { "locale": "en-US", "path": "locales/en-US.json" }
    ]
  }
}`

// TestRead_HydratesV3LocalesIntoManifestI18n verifies the post-parse step that
// flattens locales/*.json into the legacy Manifest.I18n map. The end-to-end
// path the hub relies on (handleAddonI18n returns m.I18n[lang]) is broken
// without this — FromV3.mapI18n only writes empty inner maps.
func TestRead_HydratesV3LocalesIntoManifestI18n(t *testing.T) {
	files := map[string][]byte{
		"manifest.json": []byte(v3ManifestWithLocales),
		"locales/es-MX.json": []byte(`{
			"accounting": {
				"nav": {
					"group": "Contabilidad",
					"accounts": "Catálogo de cuentas"
				},
				"model": { "account": "Cuenta contable" }
			}
		}`),
		"locales/en-US.json": []byte(`{
			"accounting": {
				"nav": {
					"group": "Accounting",
					"accounts": "Chart of accounts"
				},
				"model": { "account": "Account" }
			}
		}`),
	}

	buf := packBundle(t, files)
	b, err := Read(buf, 1<<20)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if b.Manifest.I18n == nil {
		t.Fatalf("Manifest.I18n is nil — locale inlining did not run")
	}
	// Exact bundle tags are populated.
	if got := b.Manifest.I18n["es-MX"]["accounting.nav.group"]; got != "Contabilidad" {
		t.Errorf("es-MX accounting.nav.group = %q, want %q", got, "Contabilidad")
	}
	if got := b.Manifest.I18n["es-MX"]["accounting.nav.accounts"]; got != "Catálogo de cuentas" {
		t.Errorf("es-MX accounting.nav.accounts = %q, want %q", got, "Catálogo de cuentas")
	}
	if got := b.Manifest.I18n["en-US"]["accounting.model.account"]; got != "Account" {
		t.Errorf("en-US accounting.model.account = %q, want %q", got, "Account")
	}
	// Base-language alias so a host requesting "es" still resolves a bundle
	// shipped only as "es-MX". Real-world hosts (ops i18n) normalize navigator
	// tags to the base form.
	if got := b.Manifest.I18n["es"]["accounting.nav.group"]; got != "Contabilidad" {
		t.Errorf("es alias accounting.nav.group = %q, want %q", got, "Contabilidad")
	}
	if got := b.Manifest.I18n["en"]["accounting.nav.group"]; got != "Accounting" {
		t.Errorf("en alias accounting.nav.group = %q, want %q", got, "Accounting")
	}
	// Raw locale bytes are also captured for callers that need the original file.
	if _, ok := b.Locales["locales/es-MX.json"]; !ok {
		t.Errorf("Locales does not carry the raw es-MX file")
	}
}

// TestRead_FlattenSkipsNonStringLeaves keeps malformed (numeric/array) locale
// values out of the flat map. i18next only consumes strings — leaking a
// number would crash interpolation at runtime.
func TestRead_FlattenSkipsNonStringLeaves(t *testing.T) {
	files := map[string][]byte{
		"manifest.json": []byte(v3ManifestWithLocales),
		"locales/es-MX.json": []byte(`{
			"good": "ok",
			"badNumber": 42,
			"badList": ["a","b"],
			"nested": { "good": "yes", "badBool": true }
		}`),
		"locales/en-US.json": []byte(`{"good":"ok"}`),
	}
	b, err := Read(packBundle(t, files), 1<<20)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	es := b.Manifest.I18n["es-MX"]
	if es["good"] != "ok" {
		t.Errorf("good = %q", es["good"])
	}
	if es["nested.good"] != "yes" {
		t.Errorf("nested.good = %q", es["nested.good"])
	}
	for _, k := range []string{"badNumber", "badList", "nested.badBool"} {
		if _, ok := es[k]; ok {
			t.Errorf("non-string key %q leaked into flat map", k)
		}
	}
}

// TestRead_NoLocalesBundleStillSucceeds confirms the new pass is a no-op for
// addons without an `i18n.bundles` block (most v2 addons + v3 addons that
// declare i18n purely in `metadata.i18n`).
func TestRead_NoLocalesBundleStillSucceeds(t *testing.T) {
	manifestNoI18n := []byte(`{
		"apiVersion": "asteby.com/v3",
		"kind": "Addon",
		"metadata": { "key": "no_locales", "name": "x", "version": "0.1.0", "icon": {"type":"lucide","slug":"X"} },
		"compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0" }] }
	}`)
	b, err := Read(packBundle(t, map[string][]byte{"manifest.json": manifestNoI18n}), 1<<20)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Manifest.I18n) != 0 {
		t.Errorf("Manifest.I18n unexpectedly populated: %+v", b.Manifest.I18n)
	}
}
