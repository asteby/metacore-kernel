package brand

import "github.com/asteby/metacore-kernel/brand/skin"

// Spec is the current Brand Manifest version. Bump only on a breaking
// reshape of the JSON; new optional fields are additive.
const Spec = 1

// DefaultPrefix is the public HTTP prefix every host mounts this
// handler under. Nginx/Caddy in front of Fiber (`/api` group) and chi
// (prefix-stripped `/api/` → `/`) both surface it as `{origin}/api/brand`.
const DefaultPrefix = "/api/brand"

// File is one shipped artwork blob. Type defaults to image/svg+xml when empty.
type File struct {
	Bytes  []byte
	Type   string
	Width  int
	Height int
}

// Source is the product identity a host injects at boot. Icon is required;
// Logo falls back to Icon; OG is omitted from the manifest when missing.
// Every field after OG is optional and additive (the manifest stays spec 1).
type Source struct {
	Key   string
	Name  string
	Color string
	Icon  File
	Logo  File
	OG    File

	// ShortName is the PWA short_name; "" means Name.
	ShortName   string
	Description string
	// Names holds the product name per locale (BCP-47), optional.
	Names map[string]string
	// LogoDark is the logo for dark backgrounds; empty falls back to Logo.
	LogoDark File
	// Favicon (ico/png/svg); empty falls back to Icon.
	Favicon File
	// Maskable is a PNG with the PWA "maskable" safe zone.
	Maskable File
	// Monochrome is a single-colour SVG (mask-icon, notifications).
	Monochrome File
	// Animated is an SVG animated declaratively (SMIL/CSS), never with script.
	Animated     File
	AnimatedDark File
	Theme        Theme
	Assistant    Assistant
	// StorageNamespace prefixes the client's local storage keys; "" means Key.
	StorageNamespace string
}

// Theme carries the product palette as data, so clients stop deriving a
// whole theme from one hex. Colors are #RRGGBB (#RGB is accepted and
// normalized by LoadFS).
type Theme struct {
	// Primary empty means Source.Color.
	Primary    string `json:"primary,omitempty"`
	Accent     string `json:"accent,omitempty"`
	Background string `json:"background,omitempty"`
	Foreground string `json:"foreground,omitempty"`
	// Dark is the optional dark variant.
	Dark *ThemeColors `json:"dark,omitempty"`
	// Gradient is 2-4 stops, from -> to.
	Gradient []string `json:"gradient,omitempty"`
	// ThemeColor and BackgroundColor feed the PWA manifest; empty means
	// Primary and Background.
	ThemeColor      string `json:"theme_color,omitempty"`
	BackgroundColor string `json:"background_color,omitempty"`
}

// ThemeColors is the dark (or any alternate) palette.
type ThemeColors struct {
	Primary    string `json:"primary,omitempty"`
	Accent     string `json:"accent,omitempty"`
	Background string `json:"background,omitempty"`
	Foreground string `json:"foreground,omitempty"`
}

// Assistant describes the product's AI assistant identity.
type Assistant struct {
	// Name is the default assistant name; "" means the generic one.
	Name string
	// Avatar is served at /api/brand/assistant.
	Avatar File
	// Mascot is an optional declarative skin.
	Mascot *MascotSkin
}

// MascotSkin is the declarative mascot, shared with manifest/v3 Branding.
type MascotSkin = skin.MascotSkin

// Asset is the public descriptor of one artwork, as it appears on the wire.
type Asset struct {
	URL    string `json:"url"`
	Type   string `json:"type"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// Manifest is the JSON body of GET /api/brand. Fields after Color are
// additive and omitted when empty, so a spec-1 client that only reads
// key, name, color and assets.icon/logo/og keeps working unchanged.
type Manifest struct {
	Spec   int    `json:"spec"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Color  string `json:"color,omitempty"`
	Assets struct {
		Icon         Asset  `json:"icon"`
		Logo         *Asset `json:"logo,omitempty"`
		OG           *Asset `json:"og,omitempty"`
		LogoDark     *Asset `json:"logo_dark,omitempty"`
		Favicon      *Asset `json:"favicon,omitempty"`
		Maskable     *Asset `json:"maskable,omitempty"`
		Monochrome   *Asset `json:"monochrome,omitempty"`
		Animated     *Asset `json:"animated,omitempty"`
		AnimatedDark *Asset `json:"animated_dark,omitempty"`
		Assistant    *Asset `json:"assistant,omitempty"`
	} `json:"assets"`
	ShortName        string            `json:"short_name,omitempty"`
	Description      string            `json:"description,omitempty"`
	Names            map[string]string `json:"names,omitempty"`
	StorageNamespace string            `json:"storage_namespace,omitempty"`
	// Version is a 12-hex digest of the whole identity; only emitted by a
	// handler built with WithVersionedURLs.
	Version   string             `json:"version,omitempty"`
	Theme     *Theme             `json:"theme,omitempty"`
	Assistant *AssistantManifest `json:"assistant,omitempty"`
}

// AssistantManifest is the wire form of Assistant.
type AssistantManifest struct {
	Name   string      `json:"name,omitempty"`
	Avatar *Asset      `json:"avatar,omitempty"`
	Mascot *MascotSkin `json:"mascot,omitempty"`
}

func (f File) mime() string {
	if f.Type != "" {
		return f.Type
	}
	return "image/svg+xml"
}

func (f File) present() bool {
	return len(f.Bytes) > 0
}

func (s Source) logo() File {
	if s.Logo.present() {
		return s.Logo
	}
	return s.Icon
}

func (t Theme) isZero() bool {
	return t.Primary == "" && t.Accent == "" && t.Background == "" && t.Foreground == "" &&
		t.Dark == nil && len(t.Gradient) == 0 && t.ThemeColor == "" && t.BackgroundColor == ""
}
