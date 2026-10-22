package v3

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
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
	// Login is the optional copy/hero of the sign-in page (RFC-07). Nil = the
	// host's generic login. null in an overlay removes the base's block.
	Login *LoginCopy `json:"login,omitempty"`
}

// LoginCopy is the plain-text copy of the sign-in page's brand panel. Every
// field is optional; empty falls back to the next layer of the cascade
// (organization -> instance -> preset -> generic). Text is plain: no HTML and
// no URLs are interpreted, the host renders it as text.
type LoginCopy struct {
	Eyebrow     string `json:"eyebrow,omitempty"`
	Title       string `json:"title,omitempty"`
	TitleAccent string `json:"title_accent,omitempty"`
	Subtitle    string `json:"subtitle,omitempty"`
	// Highlights replace the generic module tiles (at most 8).
	Highlights []LoginHighlight `json:"highlights,omitempty"`
	// Stats are at most 3 figures; empty or absent = NO figures are shown (the
	// host never invents numbers).
	Stats []LoginStat `json:"stats,omitempty"`
	// Signup false hides the «create account» link. Nil = the host's behavior.
	Signup *bool `json:"signup,omitempty"`
	// I18n carries per-locale overrides ("es", "en", "es-MX"), same text keys.
	I18n map[string]LoginCopyLocale `json:"i18n,omitempty"`
}

// LoginHighlight is one tile of the login panel.
type LoginHighlight struct {
	// Icon is a Lucide PascalCase name (same rule as Role.icon).
	Icon  string `json:"icon,omitempty"`
	Label string `json:"label"`
}

// LoginStat is one figure of the login panel.
type LoginStat struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// LoginCopyLocale is the translatable subset of LoginCopy.
type LoginCopyLocale struct {
	Eyebrow     string           `json:"eyebrow,omitempty"`
	Title       string           `json:"title,omitempty"`
	TitleAccent string           `json:"title_accent,omitempty"`
	Subtitle    string           `json:"subtitle,omitempty"`
	Highlights  []LoginHighlight `json:"highlights,omitempty"`
	Stats       []LoginStat      `json:"stats,omitempty"`
}

// Login copy limits (RFC-07 §2).
const (
	loginMaxShort      = 80
	loginMaxSubtitle   = 200
	loginMaxHighlights = 8
	loginMaxStats      = 3
	loginMaxLabel      = 24
	loginMaxStatValue  = 16
)

var (
	loginIconRe   = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,63}$`)
	loginLocaleRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
)

// plainText reports whether s is plain text: no angle brackets (HTML) and no
// control characters.
func plainText(s string) bool {
	if strings.ContainsAny(s, "<>") {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\t' {
			return false
		}
	}
	return true
}

func loginText(where, name, v string, max int) []string {
	var errs []string
	if n := utf8.RuneCountInString(v); n > max {
		errs = append(errs, fmt.Sprintf("%s.%s is %d characters, max %d", where, name, n, max))
	}
	if !plainText(v) {
		errs = append(errs, fmt.Sprintf("%s.%s must be plain text (no HTML, no control characters)", where, name))
	}
	return errs
}

func loginHighlights(where string, hs []LoginHighlight) []string {
	var errs []string
	if len(hs) > loginMaxHighlights {
		errs = append(errs, fmt.Sprintf("%s.highlights has %d items, max %d", where, len(hs), loginMaxHighlights))
	}
	for i, h := range hs {
		hw := fmt.Sprintf("%s.highlights[%d]", where, i)
		if h.Icon != "" && !loginIconRe.MatchString(h.Icon) {
			errs = append(errs, fmt.Sprintf("%s.icon %q must be a Lucide PascalCase name", hw, h.Icon))
		}
		if strings.TrimSpace(h.Label) == "" {
			errs = append(errs, fmt.Sprintf("%s.label is required", hw))
		}
		errs = append(errs, loginText(hw, "label", h.Label, loginMaxLabel)...)
	}
	return errs
}

func loginStats(where string, ss []LoginStat) []string {
	var errs []string
	if len(ss) > loginMaxStats {
		errs = append(errs, fmt.Sprintf("%s.stats has %d items, max %d", where, len(ss), loginMaxStats))
	}
	for i, st := range ss {
		sw := fmt.Sprintf("%s.stats[%d]", where, i)
		if strings.TrimSpace(st.Label) == "" || strings.TrimSpace(st.Value) == "" {
			errs = append(errs, fmt.Sprintf("%s needs a label and a value", sw))
		}
		errs = append(errs, loginText(sw, "label", st.Label, loginMaxLabel)...)
		errs = append(errs, loginText(sw, "value", st.Value, loginMaxStatValue)...)
	}
	return errs
}

func loginTexts(where string, eyebrow, title, accent, subtitle string) []string {
	var errs []string
	errs = append(errs, loginText(where, "eyebrow", eyebrow, loginMaxShort)...)
	errs = append(errs, loginText(where, "title", title, loginMaxShort)...)
	errs = append(errs, loginText(where, "title_accent", accent, loginMaxShort)...)
	errs = append(errs, loginText(where, "subtitle", subtitle, loginMaxSubtitle)...)
	return errs
}

// Validate checks the login copy limits: lengths, plain text, Lucide icon
// names, at most 8 highlights and 3 stats, and well-formed locale keys.
func (l *LoginCopy) Validate() []string {
	if l == nil {
		return nil
	}
	const w = "branding.login"
	errs := loginTexts(w, l.Eyebrow, l.Title, l.TitleAccent, l.Subtitle)
	errs = append(errs, loginHighlights(w, l.Highlights)...)
	errs = append(errs, loginStats(w, l.Stats)...)
	locales := make([]string, 0, len(l.I18n))
	for loc := range l.I18n {
		locales = append(locales, loc)
	}
	sort.Strings(locales)
	for _, loc := range locales {
		lw := w + ".i18n." + loc
		if !loginLocaleRe.MatchString(loc) {
			errs = append(errs, fmt.Sprintf("%s: %q is not a locale key (e.g. es, en, es-MX)", w+".i18n", loc))
		}
		t := l.I18n[loc]
		errs = append(errs, loginTexts(lw, t.Eyebrow, t.Title, t.TitleAccent, t.Subtitle)...)
		errs = append(errs, loginHighlights(lw, t.Highlights)...)
		errs = append(errs, loginStats(lw, t.Stats)...)
	}
	return errs
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
	errs = append(errs, b.Login.Validate()...)
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
