package brand

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
type Source struct {
	Key   string
	Name  string
	Color string
	Icon  File
	Logo  File
	OG    File
}

// Asset is the public descriptor of one artwork, as it appears on the wire.
type Asset struct {
	URL    string `json:"url"`
	Type   string `json:"type"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// Manifest is the JSON body of GET /api/brand.
type Manifest struct {
	Spec   int    `json:"spec"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Color  string `json:"color,omitempty"`
	Assets struct {
		Icon Asset  `json:"icon"`
		Logo *Asset `json:"logo,omitempty"`
		OG   *Asset `json:"og,omitempty"`
	} `json:"assets"`
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
