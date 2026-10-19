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
