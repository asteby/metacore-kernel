package brand

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// overrideCacheControl is used while an instance brand overrides the
// product one: short enough that a new pack shows up within minutes, and
// revalidated cheaply through the ETag.
const overrideCacheControl = "public, max-age=300"

// Layer is one level of the brand cascade. Empty fields fall through to the
// next, lower-priority layer; a File without Bytes is empty.
type Layer struct {
	// Source labels the layer ("instance", "product", ...). It is reported
	// as Resolved.Source when this is the top layer that contributed.
	Source      string
	Key         string
	Name        string
	Color       string
	AccentColor string
	MascotName  string
	Icon        File
	Logo        File
	Favicon     File
	OG          File
	// Locked: no layer above this one may override it (an issued instance
	// brand the tenant cannot replace).
	Locked bool
	// Version of the layer's content (e.g. the brand pack version), 0 if
	// not versioned.
	Version int64
}

// Resolved is the effective brand after the cascade.
type Resolved struct {
	Key         string
	Name        string
	Color       string
	AccentColor string
	MascotName  string
	Icon        File
	Logo        File
	Favicon     File
	OG          File
	// Locked is true when any applied layer is locked.
	Locked bool
	// Source and Version come from the highest-priority layer that
	// contributed at least one field.
	Source  string
	Version int64
}

// empty reports whether the layer carries no brand field at all.
func (l Layer) empty() bool {
	return l.Key == "" && l.Name == "" && l.Color == "" && l.AccentColor == "" && l.MascotName == "" &&
		!l.Icon.present() && !l.Logo.present() && !l.Favicon.present() && !l.OG.present()
}

// Cascade resolves layers ordered from HIGHEST to LOWEST priority, e.g.
// Cascade(tenant, instance, product). Each non-empty field of a higher
// layer replaces the lower one, except that nothing above a Locked layer
// is applied (the lowest locked layer wins over everything above it).
func Cascade(layers ...Layer) Resolved {
	var r Resolved
	for i := len(layers) - 1; i >= 0; i-- {
		l := layers[i]
		if l.empty() {
			continue // an empty layer neither contributes nor locks
		}
		setStr(&r.Key, l.Key)
		setStr(&r.Name, l.Name)
		setStr(&r.Color, l.Color)
		setStr(&r.AccentColor, l.AccentColor)
		setStr(&r.MascotName, l.MascotName)
		setFile(&r.Icon, l.Icon)
		setFile(&r.Logo, l.Logo)
		setFile(&r.Favicon, l.Favicon)
		setFile(&r.OG, l.OG)
		r.Source, r.Version = l.Source, l.Version
		if l.Locked {
			r.Locked = true
			break
		}
	}
	return r
}

func setStr(dst *string, v string) {
	if strings.TrimSpace(v) != "" {
		*dst = v
	}
}

func setFile(dst *File, v File) {
	if v.present() {
		*dst = v
	}
}

// Layer turns a product Source into the bottom layer of a cascade.
func (s Source) Layer() Layer {
	return Layer{Source: "product", Key: s.Key, Name: s.Name, Color: s.Color,
		Icon: s.Icon, Logo: s.Logo, OG: s.OG}
}

// AsSource converts the resolved brand back into a Source (e.g. for Handler).
func (r Resolved) AsSource() Source {
	return Source{Key: r.Key, Name: r.Name, Color: r.Color, Icon: r.Icon, Logo: r.Logo, OG: r.OG}
}

// LayerFromPack builds the "instance" layer from a VERIFIED pack (Verify
// fills PackAsset.Bytes). Missing artwork is filled from the pack's other
// assets so a customer brand never mixes with the product's artwork: icon
// falls back to favicon then logo, logo to icon then favicon, favicon to
// icon then logo.
func LayerFromPack(p *Pack) Layer {
	if p == nil {
		return Layer{}
	}
	file := func(a *PackAsset) File {
		if a == nil || len(a.Bytes) == 0 {
			return File{}
		}
		return File{Bytes: a.Bytes, Type: a.Type, Width: a.Width, Height: a.Height}
	}
	first := func(fs ...File) File {
		for _, f := range fs {
			if f.present() {
				return f
			}
		}
		return File{}
	}
	logo, icon, fav := file(p.Assets.Logo), file(p.Assets.Icon), file(p.Assets.Favicon)
	return Layer{
		Source:      "instance",
		Name:        p.Name,
		Color:       p.PrimaryColor,
		AccentColor: p.AccentColor,
		MascotName:  p.MascotName,
		Icon:        first(icon, fav, logo),
		Logo:        first(logo, icon, fav),
		Favicon:     first(fav, icon, logo),
		Locked:      p.Locked,
		Version:     p.Version,
	}
}

// Provider supplies the override layers (highest priority first) that sit
// above the product brand. It is called on every request, so it must be
// cheap (e.g. an atomic pointer load).
type Provider interface {
	BrandLayers() []Layer
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func() []Layer

// BrandLayers implements Provider.
func (f ProviderFunc) BrandLayers() []Layer { return f() }

// HandlerFrom serves the same surface as Handler, resolved per request as
// Cascade(p.BrandLayers()..., fallback.Layer()). It is meant for the
// INSTANCE brand issued by the platform; tenant (org) branding must never be
// passed here (see package doc).
//
// With no override (nil provider or only empty layers) it answers exactly
// like Handler(fallback). With an override it adds an ETag (sha256 of the
// served bytes), answers If-None-Match with 304, and caches for 5 minutes.
// It also serves {prefix}/favicon (favicon, else icon) in both modes.
func HandlerFrom(p Provider, fallback Source) http.Handler {
	static := Handler(fallback)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var layers []Layer
		if p != nil {
			layers = p.BrandLayers()
		}
		override := false
		for _, l := range layers {
			if !l.empty() {
				override = true
				break
			}
		}
		isFavicon := strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/brand/favicon")
		if !override {
			if isFavicon {
				writeFile(w, fallback.Icon)
				return
			}
			static.ServeHTTP(w, r)
			return
		}
		res := Cascade(append(append([]Layer{}, layers...), fallback.Layer())...)
		if isFavicon {
			f := res.Favicon
			if !f.present() {
				f = res.Icon
			}
			serveTagged(w, r, f)
			return
		}
		switch kind(r.URL.Path) {
		case "manifest":
			serveManifest(w, r, res)
		case "icon":
			serveTagged(w, r, res.Icon)
		case "logo":
			f := res.Logo
			if !f.present() {
				f = res.Icon
			}
			serveTagged(w, r, f)
		case "og":
			serveTagged(w, r, res.OG)
		default:
			http.NotFound(w, r)
		}
	})
}

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func notModified(w http.ResponseWriter, r *http.Request, tag string) bool {
	w.Header().Set("ETag", tag)
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == tag || t == "*" {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

func serveManifest(w http.ResponseWriter, r *http.Request, res Resolved) {
	if !res.Icon.present() {
		http.Error(w, "brand icon not configured", http.StatusNotFound)
		return
	}
	m := Manifest{Spec: Spec, Key: res.Key, Name: res.Name, Color: res.Color,
		AccentColor: res.AccentColor, Source: res.Source, Version: res.Version, Locked: res.Locked}
	m.Assets.Icon = asset(DefaultPrefix+"/icon", res.Icon)
	logo := res.Logo
	if !logo.present() {
		logo = res.Icon
	}
	l := asset(DefaultPrefix+"/logo", logo)
	m.Assets.Logo = &l
	if res.Favicon.present() {
		f := asset(DefaultPrefix+"/favicon", res.Favicon)
		m.Assets.Favicon = &f
	}
	if res.OG.present() {
		o := asset(DefaultPrefix+"/og", res.OG)
		m.Assets.OG = &o
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(m)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", overrideCacheControl)
	h.Set("Access-Control-Allow-Origin", "*")
	if notModified(w, r, etagOf(buf.Bytes())) {
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

func serveTagged(w http.ResponseWriter, r *http.Request, f File) {
	if !f.present() {
		http.Error(w, "brand asset not found", http.StatusNotFound)
		return
	}
	mime := f.mime()
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", overrideCacheControl)
	if strings.HasPrefix(mime, "image/svg") {
		h.Set("Content-Security-Policy", svgCSP)
	}
	if notModified(w, r, etagOf(f.Bytes)) {
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(f.Bytes)
	}
}
