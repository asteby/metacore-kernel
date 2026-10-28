package v3

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseBranding_RejectsUnknownAndNonString(t *testing.T) {
	if _, errs := ParseBranding(map[string]interface{}{"platform_name": "X", "slogan": "y"}); len(errs) == 0 || !strings.Contains(errs[0], "slogan") {
		t.Fatalf("unknown key: got %v", errs)
	}
	if _, errs := ParseBranding(map[string]interface{}{"primary_color": 12}); len(errs) == 0 {
		t.Fatal("non-string value should fail")
	}
	if _, errs := ParseBranding("Pitsline"); len(errs) == 0 {
		t.Fatal("non-object should fail")
	}
	b, errs := ParseBranding(map[string]interface{}{"platform_name": "X", "mascot_name": nil})
	if len(errs) != 0 || b.PlatformName != "X" {
		t.Fatalf("null field (merge-patch removal) should parse: %v %+v", errs, b)
	}
}

func TestBranding_Validate(t *testing.T) {
	ok := Branding{
		PrimaryColor: "#EB2221", AccentColor: "#fff", LogoURL: "./assets/logo.svg",
		LogoDarkURL: "https://cdn.example.com/dark.svg", LogoMarkURL: "./assets/brand/mark.png",
		SupportEmail: "hola@example.com", SupportURL: "https://example.com",
	}
	if errs := ok.Validate(); len(errs) != 0 {
		t.Fatalf("valid branding: %v", errs)
	}
	cases := []struct {
		b    Branding
		want string
	}{
		{Branding{PrimaryColor: "red"}, "primary_color"},
		{Branding{AccentColor: "#12345"}, "accent_color"},
		{Branding{LogoURL: "logo.svg"}, "logo_url"},
		{Branding{FaviconURL: "./defaults/icon.svg"}, "favicon_url"},
		{Branding{LoginBackgroundURL: "./assets/../manifest.json"}, "login_background_url"},
		{Branding{LogoMarkURL: "./assets/"}, "logo_mark_url"},
		{Branding{SupportEmail: "nope"}, "support_email"},
		{Branding{SupportURL: "mailto:x@y.z"}, "support_url"},
	}
	for _, c := range cases {
		errs := c.b.Validate()
		if len(errs) != 1 || !strings.Contains(errs[0], c.want) {
			t.Errorf("%+v: want one %s error, got %v", c.b, c.want, errs)
		}
	}
}

func TestBranding_AssetPathsAndRewrite(t *testing.T) {
	b := Branding{
		PlatformName: "P", LogoURL: "./assets/logo.svg", FaviconURL: "./assets/logo.svg",
		LogoDarkURL: "https://cdn/x.svg", LoginBackgroundURL: "./assets/bg.jpg",
	}
	if got, want := b.AssetPaths(), []string{"assets/bg.jpg", "assets/logo.svg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AssetPaths = %v, want %v", got, want)
	}
	r := b.WithAssetURLs(map[string]string{"assets/logo.svg": "https://s3/org/logo.svg"})
	if r.LogoURL != "https://s3/org/logo.svg" || r.FaviconURL != "https://s3/org/logo.svg" {
		t.Fatalf("rewrite: %+v", r)
	}
	if r.LoginBackgroundURL != "./assets/bg.jpg" || r.LogoDarkURL != "https://cdn/x.svg" {
		t.Fatalf("unmapped fields must stay: %+v", r)
	}
	if b.LogoURL != "./assets/logo.svg" {
		t.Fatal("WithAssetURLs mutated the receiver")
	}
	m := r.ToMap()
	if _, has := m["accent_color"]; has || m["platform_name"] != "P" || len(m) != 5 {
		t.Fatalf("ToMap must omit empty fields: %v", m)
	}
}

func TestValidate_PresetInlineBranding(t *testing.T) {
	m := presetManifest(map[string]interface{}{
		"addons":   []interface{}{map[string]interface{}{"key": "pos", "version": "^0.1.0"}},
		"defaults": map[string]interface{}{"branding": map[string]interface{}{"primary_color": "rojo"}},
	})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "primary_color") {
		t.Fatalf("want primary_color error, got %v", err)
	}
	m = presetManifest(map[string]interface{}{
		"addons":   []interface{}{map[string]interface{}{"key": "pos", "version": "^0.1.0"}},
		"defaults": map[string]interface{}{"branding_file": "./defaults/branding.json", "branding": map[string]interface{}{"logo_mark_url": "./assets/mark.svg", "pwa_short_name": "P"}},
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("valid inline branding: %v", err)
	}
}

func TestBranding_Fonts(t *testing.T) {
	if errs := (Branding{Fonts: []string{"Barlow", "Barlow Condensed"}}).Validate(); len(errs) != 0 {
		t.Fatalf("valid fonts: %v", errs)
	}
	if errs := (Branding{Fonts: []string{"Barlow;}</style>"}}).Validate(); len(errs) != 1 {
		t.Fatalf("want one fonts error, got %v", errs)
	}
	b, errs := ParseBranding(map[string]interface{}{"fonts": []interface{}{"Barlow"}})
	if len(errs) != 0 || len(b.Fonts) != 1 || b.ToMap()["fonts"] == nil {
		t.Fatalf("parse fonts: %v %+v", errs, b)
	}
}

func skinFixture() map[string]interface{} {
	return map[string]interface{}{
		"id": "hex-navy", "name": "Asistente", "tooth_shape": "hex-frame", "antenna": false, "screen_tone": "dark",
		"colors": map[string]interface{}{"tire": "#051C35", "tread_or_tooth": "#102E6B", "accent": "#2160DE", "glow": "#628FEA", "screen": "#041324", "screen_center": "#071A36"},
		"face":   map[string]interface{}{"stroke": "#f4f8ff", "stroke_width": 6.5, "tread_dot": "#f4f8ff"},
	}
}

func TestBranding_Mascot(t *testing.T) {
	b, errs := ParseBranding(map[string]interface{}{"mascot": skinFixture(), "mascot_skin": "gear-lime"})
	if len(errs) != 0 || b.Mascot == nil || b.Mascot.ID != "hex-navy" || b.Mascot.Face == nil || b.Mascot.Face.StrokeWidth != 6.5 {
		t.Fatalf("parse mascot: %v %+v", errs, b)
	}
	if errs := b.Validate(); len(errs) != 0 {
		t.Fatalf("valid mascot: %v", errs)
	}
	m := b.ToMap()
	if _, ok := m["mascot"].(map[string]interface{}); !ok || m["mascot_skin"] != "gear-lime" {
		t.Fatalf("ToMap: %v", m)
	}

	bad := skinFixture()
	bad["tooth_shape"] = "star"
	b, errs = ParseBranding(map[string]interface{}{"mascot": bad})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if errs := b.Validate(); len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), "branding.mascot: tooth_shape") {
		t.Fatalf("want a tooth_shape error, got %v", errs)
	}
	if _, errs := ParseBranding(map[string]interface{}{"mascot": map[string]interface{}{"colour": "x"}}); len(errs) == 0 {
		t.Fatal("unknown mascot key accepted")
	}
}

func TestValidate_PresetBrandingMascotSchema(t *testing.T) {
	preset := func(mascot interface{}) []byte {
		return mustJSON(t, presetManifest(map[string]interface{}{
			"addons":   []interface{}{map[string]interface{}{"key": "pos", "version": "^0.1.0"}},
			"defaults": map[string]interface{}{"branding": map[string]interface{}{"mascot": mascot}},
		}))
	}
	if err := Validate(preset(skinFixture())); err != nil {
		t.Fatalf("valid mascot in schema: %v", err)
	}
	if err := Validate(preset(nil)); err != nil {
		t.Fatalf("null mascot (removes the base's): %v", err)
	}
	bad := skinFixture()
	bad["tooth_shape"] = "star"
	if err := Validate(preset(bad)); err == nil {
		t.Fatal("schema accepted tooth_shape star")
	}
	bad = skinFixture()
	delete(bad, "colors")
	if err := Validate(preset(bad)); err == nil {
		t.Fatal("schema accepted a skin without colors")
	}
}
