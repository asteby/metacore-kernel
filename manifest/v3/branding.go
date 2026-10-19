package v3

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Branding is the typed brand kit a preset ships in preset.defaults.branding
// (inline or via branding_file). Every field is optional; the host applies the
// non-empty ones to the organization. Asset fields (the *_url ones) hold
// either an absolute http(s) URL or a bundle-relative "./assets/..." path that
// the host materializes from the preset bundle and rewrites to a URL
// (WithAssetURLs).
type Branding struct {
	PlatformName string `json:"platform_name,omitempty"`
	// LogoURL is the main (light background) logo.
	LogoURL string `json:"logo_url,omitempty"`
	// LogoDarkURL is the logo for dark backgrounds. Empty = LogoURL.
	LogoDarkURL string `json:"logo_dark_url,omitempty"`
	// LogoMarkURL is the square icon (collapsed sidebar, PWA icon, avatar).
	LogoMarkURL        string `json:"logo_mark_url,omitempty"`
	FaviconURL         string `json:"favicon_url,omitempty"`
	LoginBackgroundURL string `json:"login_background_url,omitempty"`
	// PrimaryColor and AccentColor are hex colors: #RGB, #RRGGBB or #RRGGBBAA.
	PrimaryColor string `json:"primary_color,omitempty"`
	AccentColor  string `json:"accent_color,omitempty"`
	SupportEmail string `json:"support_email,omitempty"`
	SupportURL   string `json:"support_url,omitempty"`
	MascotName   string `json:"mascot_name,omitempty"`
	MascotSkin   string `json:"mascot_skin,omitempty"`
	// ThemeKey, Font and Density mirror preset.defaults.ui_theme; a host
	// reading both lets branding win.
	ThemeKey string `json:"theme_key,omitempty"`
	Font     string `json:"font,omitempty"`
	Density  string `json:"density,omitempty"`
	// Fonts are extra font families (Google Fonts names, e.g. "Barlow") the
	// host loads for the organization's own surfaces (kiosk, customer
	// display, documents) besides the UI Font token.
	Fonts []string `json:"fonts,omitempty"`
	// PWAName and PWAShortName feed the web app manifest. Empty = PlatformName.
	PWAName      string `json:"pwa_name,omitempty"`
	PWAShortName string `json:"pwa_short_name,omitempty"`
	// EmailFromName is the display name of outgoing mail. Empty = PlatformName.
	EmailFromName string `json:"email_from_name,omitempty"`
}

// BrandingAssetPrefix is the bundle directory branding assets live in.
const BrandingAssetPrefix = "./assets/"

var fontFamily = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ]{0,59}$`)

var hexColor = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// ParseBranding decodes a preset.defaults.branding value (the decoded JSON
// object). Unknown keys and non-string values are reported as errors.
func ParseBranding(v interface{}) (Branding, []string) {
	var b Branding
	obj, ok := v.(map[string]interface{})
	if !ok {
		return b, []string{"branding must be an object"}
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return b, []string{fmt.Sprintf("branding: %v", err)}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Branding{}, []string{fmt.Sprintf("branding: %v", err)}
	}
	return b, nil
}

// Validate checks colors are hex, asset fields are an http(s) URL or a clean
// "./assets/..." path, support_email is an address and support_url is http(s).
func (b Branding) Validate() []string {
	var errs []string
	for _, c := range []struct{ name, v string }{{"primary_color", b.PrimaryColor}, {"accent_color", b.AccentColor}} {
		if c.v != "" && !hexColor.MatchString(c.v) {
			errs = append(errs, fmt.Sprintf("branding.%s %q is not a hex color (#RGB, #RRGGBB or #RRGGBBAA)", c.name, c.v))
		}
	}
	for _, a := range b.assetFields() {
		if *a.v == "" || isHTTPURL(*a.v) {
			continue
		}
		if _, ok := bundleAssetPath(*a.v); !ok {
			errs = append(errs, fmt.Sprintf("branding.%s %q must be an http(s) URL or a %s... path inside the bundle", a.name, *a.v, BrandingAssetPrefix))
		}
	}
	for _, f := range b.Fonts {
		if !fontFamily.MatchString(f) {
			errs = append(errs, fmt.Sprintf("branding.fonts %q is not a font family name (letters, digits and spaces)", f))
		}
	}
	if b.SupportEmail != "" {
		if _, err := mail.ParseAddress(b.SupportEmail); err != nil {
			errs = append(errs, fmt.Sprintf("branding.support_email %q is not an email address", b.SupportEmail))
		}
	}
	if b.SupportURL != "" && !isHTTPURL(b.SupportURL) {
		errs = append(errs, fmt.Sprintf("branding.support_url %q must be an http(s) URL", b.SupportURL))
	}
	return errs
}

// AssetPaths returns the bundle paths (without "./", e.g. "assets/logo.svg")
// the asset fields reference, sorted and de-duplicated. Absolute URLs are not
// listed.
func (b Branding) AssetPaths() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, a := range b.assetFields() {
		p, ok := bundleAssetPath(*a.v)
		if !ok {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// WithAssetURLs returns a copy whose bundle-relative asset fields are replaced
// by urls[<bundle path>]. Fields whose path is not in urls are left as is.
func (b Branding) WithAssetURLs(urls map[string]string) Branding {
	out := b
	for _, a := range out.assetFields() {
		if p, ok := bundleAssetPath(*a.v); ok {
			if u, found := urls[p]; found {
				*a.v = u
			}
		}
	}
	return out
}

// ToMap returns the non-empty fields keyed by their JSON name, the shape hosts
// store as the organization's branding.
func (b Branding) ToMap() map[string]interface{} {
	raw, _ := json.Marshal(b)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return out
}

type brandingAssetField struct {
	name string
	v    *string
}

func (b *Branding) assetFields() []brandingAssetField {
	return []brandingAssetField{
		{"logo_url", &b.LogoURL},
		{"logo_dark_url", &b.LogoDarkURL},
		{"logo_mark_url", &b.LogoMarkURL},
		{"favicon_url", &b.FaviconURL},
		{"login_background_url", &b.LoginBackgroundURL},
	}
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// BundleAssetPath maps a "./assets/..." reference to its bundle path
// ("assets/..."); ok is false for anything else (URLs, other dirs, escapes).
func BundleAssetPath(ref string) (string, bool) { return bundleAssetPath(ref) }

// bundleAssetPath maps "./assets/x/logo.svg" to "assets/x/logo.svg". It rejects
// anything outside ./assets/ (including ".." escapes) and the bare directory.
func bundleAssetPath(ref string) (string, bool) {
	if !strings.HasPrefix(ref, BrandingAssetPrefix) {
		return "", false
	}
	rel := strings.TrimPrefix(ref, "./")
	if path.Clean(rel) != rel || strings.ContainsAny(rel, "\\%") || rel == "assets" {
		return "", false
	}
	return rel, true
}
