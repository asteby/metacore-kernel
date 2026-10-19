package preset

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func pitslineBundleFiles() map[string][]byte {
	return map[string][]byte{
		"defaults/branding.json": []byte(`{"platform_name": "Pitsline", "logo_url": "./assets/logo.svg", "favicon_url": "./assets/logo.svg", "primary_color": "#EB2221", "mascot_name": "Llantonio"}`),
		"defaults/roles.json":    []byte(`[{"name": "cajero", "label": "Cajero", "permissions": ["pos.session.open"]}]`),
		"assets/logo.svg":        []byte(`<svg id="pitsline"/>`),
	}
}

func pitslineFileManifest() *v3.Manifest {
	return &v3.Manifest{
		Kind:     v3.KindPreset,
		Metadata: v3.Metadata{Key: "pitsline", Name: "Pitsline", Version: "0.9.3"},
		Preset: &v3.Preset{
			Addons: []v3.PresetAddon{{Key: "pos", Version: "^0.42.0"}},
			Defaults: map[string]interface{}{
				"branding_file":   "./defaults/branding.json",
				"roles_file":      "./defaults/roles.json",
				"dashboards_file": "./defaults/dashboards.json",
				"ui_theme":        map[string]interface{}{"theme_key": "classic", "font": "manrope", "density": "comfortable"},
			},
		},
	}
}

func TestReadFileFrom(t *testing.T) {
	read := ReadFileFrom(pitslineBundleFiles())
	if b, err := read("./assets/logo.svg"); err != nil || !strings.Contains(string(b), "pitsline") {
		t.Fatalf("read ./assets/logo.svg: %q %v", b, err)
	}
	for _, p := range []string{"assets/missing.svg", "assets/../defaults/roles.json", ""} {
		if _, err := read(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q: want fs.ErrNotExist, got %v", p, err)
		}
	}
}

func TestResolveWithFiles_InlinesAndBrands(t *testing.T) {
	r, missing, err := ResolveWithFiles(pitslineFileManifest(), pitslineBundleFiles())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []string{"dashboards_file"}) {
		t.Fatalf("missing = %v", missing)
	}
	if _, ok := r.Defaults["roles"].([]interface{}); !ok {
		t.Fatalf("roles not inlined: %v", r.Defaults["roles"])
	}
	b, problems := r.Branding()
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if b.PlatformName != "Pitsline" || b.ThemeKey != "classic" || b.Font != "manrope" || b.Density != "comfortable" {
		t.Fatalf("branding = %+v", b)
	}
	if got := b.AssetPaths(); !reflect.DeepEqual(got, []string{"assets/logo.svg"}) || r.Files[got[0]] == nil {
		t.Fatalf("asset paths %v not backed by Files", got)
	}
}

func TestResolvedBranding_ReportsMissingAssetAndInvalid(t *testing.T) {
	r := Resolved{
		Defaults: map[string]interface{}{"branding": map[string]interface{}{"logo_url": "./assets/nope.svg", "primary_color": "#EB2221"}},
		Files:    map[string][]byte{},
	}
	if _, problems := r.Branding(); len(problems) != 1 || !strings.Contains(problems[0], "assets/nope.svg") {
		t.Fatalf("problems = %v", problems)
	}
	r.Defaults["branding"] = map[string]interface{}{"primary_color": "red"}
	if _, problems := r.Branding(); len(problems) != 1 || !strings.Contains(problems[0], "primary_color") {
		t.Fatalf("problems = %v", problems)
	}
	// Without Files (bare manifest) asset existence is not checked.
	r = Resolved{Defaults: map[string]interface{}{"branding": map[string]interface{}{"logo_url": "./assets/nope.svg"}}}
	if _, problems := r.Branding(); len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	// ui_theme only fills what branding leaves empty.
	r = Resolved{Defaults: map[string]interface{}{
		"branding": map[string]interface{}{"font": "inter"},
		"ui_theme": map[string]interface{}{"font": "manrope", "theme_key": "classic"},
	}}
	if b, _ := r.Branding(); b.Font != "inter" || b.ThemeKey != "classic" {
		t.Fatalf("branding = %+v", b)
	}
}

func TestFlattenWithFiles_BrandingAndAssets(t *testing.T) {
	base, _, err := ResolveWithFiles(pitslineFileManifest(), pitslineBundleFiles())
	if err != nil {
		t.Fatal(err)
	}
	overlay := &v3.Manifest{
		Kind:     v3.KindPreset,
		Metadata: v3.Metadata{Key: "sieteleguas", Name: "Siete Leguas", Version: "1.0.0", Visibility: v3.VisibilityPrivate},
		Preset: &v3.Preset{
			Extends:  &v3.PresetExtends{Key: "pitsline", Version: ">=0.9.0 <1.0.0"},
			Defaults: map[string]interface{}{"branding_file": "./defaults/branding.json"},
		},
	}
	overlayFiles := map[string][]byte{
		"defaults/branding.json": []byte(`{"platform_name": "Siete Leguas", "logo_mark_url": "./assets/mark.svg", "mascot_name": null}`),
		"assets/logo.svg":        []byte(`<svg id="sieteleguas"/>`),
		"assets/mark.svg":        []byte(`<svg id="mark"/>`),
	}
	r, missing, err := FlattenWithFiles(base, overlay, overlayFiles)
	if err != nil || len(missing) != 0 {
		t.Fatalf("flatten: %v %v", err, missing)
	}
	b, problems := r.Branding()
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if b.PlatformName != "Siete Leguas" || b.PrimaryColor != "#EB2221" || b.MascotName != "" || b.LogoURL != "./assets/logo.svg" {
		t.Fatalf("merge-patch branding = %+v", b)
	}
	if string(r.Files["assets/logo.svg"]) != `<svg id="sieteleguas"/>` {
		t.Fatalf("overlay asset must win: %s", r.Files["assets/logo.svg"])
	}
	if r.Files["defaults/roles.json"] == nil || r.Files["assets/mark.svg"] == nil {
		t.Fatal("files must be the union of base and overlay")
	}
	if string(base.Files["assets/logo.svg"]) != `<svg id="pitsline"/>` {
		t.Fatal("FlattenWithFiles mutated the base files")
	}
	if overlay.Preset.Defaults["branding"] != nil {
		t.Fatal("FlattenWithFiles mutated the overlay manifest")
	}
}

func TestInlineFiles_UnwrapsSelfNamedObject(t *testing.T) {
	files := map[string][]byte{
		"defaults/roles.json":    []byte(`{"roles": [{"name": "fleet_manager"}]}`),
		"defaults/branding.json": []byte(`{"platform_name": "P"}`),
	}
	out, missing := InlineFiles(map[string]interface{}{
		"roles_file":    "./defaults/roles.json",
		"branding_file": "./defaults/branding.json",
	}, ReadFileFrom(files))
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if roles, ok := out["roles"].([]interface{}); !ok || len(roles) != 1 {
		t.Fatalf("roles not unwrapped: %#v", out["roles"])
	}
	if b, ok := out["branding"].(map[string]interface{}); !ok || b["platform_name"] != "P" {
		t.Fatalf("branding (not wrapped) changed: %#v", out["branding"])
	}
}
