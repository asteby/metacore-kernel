package bundle

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func pitslinePresetEntries(t *testing.T) map[string][]byte {
	t.Helper()
	mf, err := os.ReadFile("../preset/testdata/pitsline_preset.json")
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{
		"manifest.json":            mf,
		"defaults/roles.json":      []byte(`[]`),
		"defaults/categories.json": []byte(`[]`),
		"defaults/dashboards.json": []byte(`[]`),
		"defaults/branding.json":   []byte(`{"platform_name": "Pitsline", "logo_url": "./assets/logo.svg", "primary_color": "#EB2221"}`),
		"assets/logo.svg":          []byte(`<svg/>`),
	}
}

func TestRead_PresetFilesKept(t *testing.T) {
	b, err := Read(packBundle(t, pitslinePresetEntries(t)), 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b.PresetFiles) != 5 || string(b.PresetFiles["assets/logo.svg"]) != "<svg/>" {
		t.Fatalf("PresetFiles = %v", b.PresetFiles)
	}
	if data, err := b.ReadPresetFile("./defaults/branding.json"); err != nil || !bytes.Contains(data, []byte("Pitsline")) {
		t.Fatalf("ReadPresetFile: %q %v", data, err)
	}
}

func TestRead_PresetMissingAssetOrFile(t *testing.T) {
	cases := map[string]func(map[string][]byte){
		"assets/logo.svg":          func(e map[string][]byte) { delete(e, "assets/logo.svg") },
		"dashboards_file":          func(e map[string][]byte) { delete(e, "defaults/dashboards.json") },
		"branding.primary_color":   func(e map[string][]byte) { e["defaults/branding.json"] = []byte(`{"primary_color": "rojo"}`) },
		"unknown field \"slogan\"": func(e map[string][]byte) { e["defaults/branding.json"] = []byte(`{"slogan": "x"}`) },
		"./assets/kiosk.png": func(e map[string][]byte) {
			e["defaults/dashboards.json"] = []byte(`[{"key": "k", "image": "./assets/kiosk.png"}]`)
		},
	}
	for want, mutate := range cases {
		entries := pitslinePresetEntries(t)
		mutate(entries)
		_, err := Read(packBundle(t, entries), 0)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("strict %s: got %v", want, err)
		}
		b, err := ReadWithOptions(packBundle(t, entries), 0, ReadOptions{})
		if err != nil {
			t.Fatalf("non-strict %s: %v", want, err)
		}
		if len(b.ValidationWarnings) == 0 || !strings.Contains(strings.Join(b.ValidationWarnings, "; "), want) {
			t.Errorf("non-strict %s: warnings %v", want, b.ValidationWarnings)
		}
	}
}

func TestWrite_PresetFilesRoundTrip(t *testing.T) {
	src, err := Read(packBundle(t, pitslinePresetEntries(t)), 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, src); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PresetFiles) != len(src.PresetFiles) || string(got.PresetFiles["assets/logo.svg"]) != "<svg/>" {
		t.Fatalf("round trip lost preset files: %v", got.PresetFiles)
	}
	src.PresetFiles["frontend/x.js"] = []byte("x")
	if err := Write(&bytes.Buffer{}, src); err == nil {
		t.Fatal("Write must reject a preset file outside defaults/ or assets/")
	}
}

// r1 on #508: SVG assets are validated, unsafe paths are rejected early, and
// unclean ./assets/ refs and shadowed *_file keys are reported.
func TestRead_PresetHardening(t *testing.T) {
	evil := pitslinePresetEntries(t)
	evil["assets/logo.svg"] = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if _, err := Read(packBundle(t, evil), 0); err == nil || !strings.Contains(err.Error(), "assets/logo.svg") {
		t.Fatalf("malicious svg (strict): %v", err)
	}
	b, err := ReadWithOptions(packBundle(t, evil), 0, ReadOptions{})
	if err != nil || !strings.Contains(strings.Join(b.ValidationWarnings, ";"), "svg not allowed") {
		t.Fatalf("malicious svg (lenient): %v %v", err, b)
	}
	ok := pitslinePresetEntries(t)
	ok["assets/logo.svg"] = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0"/></svg>`)
	if _, err := Read(packBundle(t, ok), 0); err != nil {
		t.Fatalf("clean svg: %v", err)
	}

	for _, name := range []string{`assets\logo.svg`, "assets/%2e%2e/x.svg", `frontend\x.js`} {
		e := pitslinePresetEntries(t)
		e[name] = []byte("x")
		if _, err := ReadWithOptions(packBundle(t, e), 0, ReadOptions{}); err == nil {
			t.Errorf("%q must be rejected even when lenient", name)
		}
	}

	unclean := pitslinePresetEntries(t)
	unclean["defaults/dashboards.json"] = []byte(`[{"key": "k", "image": "./assets/../manifest.json"}]`)
	if _, err := Read(packBundle(t, unclean), 0); err == nil || !strings.Contains(err.Error(), "not a valid ./assets/ path") {
		t.Fatalf("unclean ref: %v", err)
	}
}

func TestWrite_RejectsEscapingPresetFile(t *testing.T) {
	src, err := Read(packBundle(t, pitslinePresetEntries(t)), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"assets/../x", "assets/%2e%2e/x", `assets\x`} {
		b := *src
		b.PresetFiles = map[string][]byte{k: []byte("x")}
		if err := Write(&bytes.Buffer{}, &b); err == nil {
			t.Errorf("Write accepted %q", k)
		}
	}
}
