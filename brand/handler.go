package brand

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cacheControl = "public, max-age=86400"
	svgCSP       = "sandbox; default-src 'none'; style-src 'unsafe-inline'"

	defaultMaxAge = 24 * time.Hour
	immutableAge  = 365 * 24 * time.Hour
)

// Provider returns the current product identity. A host that lets operators
// override the brand per instance implements it over its own store; most hosts
// use Static.
type Provider interface {
	Brand() Source
}

type staticProvider struct{ src Source }

func (p staticProvider) Brand() Source { return p.src }

// Static is a Provider that always returns src. NewHandler recognizes it and
// builds the manifest, hashes and ETags once.
func Static(src Source) Provider { return staticProvider{src: src} }

// Option tunes NewHandler.
type Option func(*options)

type options struct {
	manifestAge, assetAge time.Duration
	versioned             bool
	version               func() string
}

// WithMaxAge sets the Cache-Control max-age of the manifest and of the assets
// (default 24 h each). An asset requested with ?v=<current version> is always
// served immutable for a year, whatever this says.
func WithMaxAge(manifest, assets time.Duration) Option {
	return func(o *options) { o.manifestAge, o.assetAge = manifest, assets }
}

// WithVersionedURLs makes the manifest carry "version" (a 12-hex digest of the
// whole identity) and append ?v=<version> to every asset URL, so browsers can
// cache assets forever and still pick up a new brand. Without it the manifest
// is byte-identical to the original spec-1 output.
func WithVersionedURLs() Option { return func(o *options) { o.versioned = true } }

// WithVersion gives a dynamic Provider a cheap change token (for example the
// sha of the instance's brand pack). While it returns the same string the
// handler reuses the manifest and hashes it already computed instead of
// rebuilding them on every request. The token is also what the version and
// the ?v= value derive from, so a new token always changes both.
func WithVersion(fn func() string) Option { return func(o *options) { o.version = fn } }

// Handler serves the brand manifest and its artwork for a fixed Source. It
// matches on the path suffix so the same handler works mounted at /brand (chi
// behind an /api/ strip) or /api/brand (Fiber group). It is
// NewHandler(Static(src)).
func Handler(src Source) http.Handler { return NewHandler(Static(src)) }

// NewHandler is Handler over a Provider, with options. It never validates the
// Source (a misconfigured host still serves what it has); call Source.Validate
// at boot to fail fast.
func NewHandler(p Provider, opts ...Option) http.Handler {
	h := &handler{p: p, o: options{manifestAge: defaultMaxAge, assetAge: defaultMaxAge}}
	for _, opt := range opts {
		opt(&h.o)
	}
	_, h.static = p.(staticProvider)
	return h
}

type handler struct {
	p      Provider
	o      options
	static bool

	mu    sync.Mutex
	snap  *snapshot
	token string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	k := kind(r.URL.Path)
	if k == "" {
		http.NotFound(w, r)
		return
	}
	snap := h.snapshot()
	if k == "manifest" {
		h.serveManifest(w, r, snap)
		return
	}
	e, ok := snap.assets[k]
	if !ok {
		http.Error(w, "brand asset not found", http.StatusNotFound)
		return
	}
	h.serveAsset(w, r, snap, e)
}

// kinds maps the last path segment after /brand/ to an asset kind.
var kinds = map[string]bool{
	"icon": true, "logo": true, "og": true, "logo-dark": true, "favicon": true,
	"maskable": true, "monochrome": true, "animated": true, "animated-dark": true, "assistant": true,
}

func kind(path string) string {
	p := strings.TrimSuffix(path, "/")
	if strings.HasSuffix(p, "/brand") {
		return "manifest"
	}
	i := strings.LastIndex(p, "/brand/")
	if i < 0 {
		return ""
	}
	if seg := p[i+len("/brand/"):]; kinds[seg] {
		return seg
	}
	return ""
}

type entry struct {
	file File
	etag string
}

type snapshot struct {
	version      string
	manifest     []byte
	manifestETag string
	hasIcon      bool
	assets       map[string]entry
}

func (h *handler) snapshot() *snapshot {
	if h.static {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.snap == nil {
			h.snap = build(h.p.Brand(), "", h.o.versioned)
		}
		return h.snap
	}
	if h.o.version == nil {
		return build(h.p.Brand(), "", h.o.versioned)
	}
	token := h.o.version()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snap == nil || h.token != token {
		h.snap, h.token = build(h.p.Brand(), token, h.o.versioned), token
	}
	return h.snap
}

func (h *handler) serveManifest(w http.ResponseWriter, r *http.Request, s *snapshot) {
	if !s.hasIcon {
		http.Error(w, "brand icon not configured", http.StatusNotFound)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json; charset=utf-8")
	hd.Set("Cache-Control", maxAge(h.o.manifestAge))
	hd.Set("Access-Control-Allow-Origin", "*")
	hd.Set("ETag", s.manifestETag)
	if etagMatches(r.Header.Get("If-None-Match"), s.manifestETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(s.manifest)
	}
}

func (h *handler) serveAsset(w http.ResponseWriter, r *http.Request, s *snapshot, e entry) {
	hd := w.Header()
	hd.Set("Content-Type", e.file.mime())
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", svgCSP)
	hd.Set("Access-Control-Allow-Origin", "*")
	if v := r.URL.Query().Get("v"); v != "" && v == s.version {
		hd.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(immutableAge/time.Second))+", immutable")
	} else {
		hd.Set("Cache-Control", maxAge(h.o.assetAge))
	}
	hd.Set("ETag", e.etag)
	if etagMatches(r.Header.Get("If-None-Match"), e.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(e.file.Bytes)
	}
}

func maxAge(d time.Duration) string {
	if d == defaultMaxAge {
		return cacheControl
	}
	if d < 0 {
		d = 0
	}
	return "public, max-age=" + strconv.Itoa(int(d/time.Second))
}

// etagMatches implements the weak comparison of If-None-Match: a list of
// entity tags (optionally W/-prefixed) or "*".
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, c := range strings.Split(header, ",") {
		c = strings.TrimSpace(c)
		if c == "*" || strings.TrimPrefix(c, "W/") == etag {
			return true
		}
	}
	return false
}

// resolved returns the files served per kind, with the fallbacks of the
// contract: logo -> icon, logo-dark -> logo -> icon, favicon -> icon.
func resolved(src Source) map[string]File {
	m := map[string]File{}
	put := func(k string, fs ...File) {
		for _, f := range fs {
			if f.present() {
				m[k] = f
				return
			}
		}
	}
	put("icon", src.Icon)
	put("logo", src.Logo, src.Icon)
	put("og", src.OG)
	put("logo-dark", src.LogoDark, src.Logo, src.Icon)
	put("favicon", src.Favicon, src.Icon)
	put("maskable", src.Maskable)
	put("monochrome", src.Monochrome)
	put("animated", src.Animated)
	put("animated-dark", src.AnimatedDark)
	put("assistant", src.Assistant.Avatar)
	return m
}

func sum(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

func hex12(b []byte) string { return hex.EncodeToString(b)[:12] }

func build(src Source, token string, versioned bool) *snapshot {
	files := resolved(src)
	s := &snapshot{hasIcon: src.Icon.present(), assets: make(map[string]entry, len(files))}
	for k, f := range files {
		s.assets[k] = entry{file: f, etag: `"` + hex12(sum([]byte(f.mime()), f.Bytes)) + `"`}
	}
	if !s.hasIcon {
		return s
	}
	// The version covers the manifest (without version and ?v=) and every asset.
	// Kinds are visited in a fixed order so the digest is stable.
	body := encode(manifestFor(src, files, "", false))
	parts := [][]byte{body}
	for _, k := range []string{"icon", "logo", "og", "logo-dark", "favicon", "maskable", "monochrome", "animated", "animated-dark", "assistant"} {
		if e, ok := s.assets[k]; ok {
			parts = append(parts, []byte(k+":"+e.etag))
		}
	}
	if token != "" {
		parts = append(parts, []byte("token:"+token))
	}
	s.version = hex12(sum(parts...))
	if versioned {
		body = encode(manifestFor(src, files, s.version, true))
	}
	s.manifest = body
	s.manifestETag = `"` + hex12(sum(body)) + `"`
	return s
}

func encode(m Manifest) []byte {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(m)
	return buf.Bytes()
}

func manifestFor(src Source, files map[string]File, version string, versioned bool) Manifest {
	url := func(k string) string {
		u := DefaultPrefix + "/" + k
		if versioned {
			u += "?v=" + version
		}
		return u
	}
	ptr := func(k string) *Asset {
		f, ok := files[k]
		if !ok {
			return nil
		}
		a := asset(url(k), f)
		return &a
	}
	own := func(k string, f File) *Asset { // only when the product ships its own file
		if !f.present() {
			return nil
		}
		return ptr(k)
	}
	m := Manifest{Spec: Spec, Key: src.Key, Name: src.Name, Color: src.Color}
	if m.Color == "" {
		m.Color = src.Theme.Primary
	}
	m.Assets.Icon = asset(url("icon"), src.Icon)
	m.Assets.Logo = ptr("logo")
	m.Assets.OG = ptr("og")
	m.Assets.LogoDark = own("logo-dark", src.LogoDark)
	m.Assets.Favicon = own("favicon", src.Favicon)
	m.Assets.Maskable = ptr("maskable")
	m.Assets.Monochrome = ptr("monochrome")
	m.Assets.Animated = ptr("animated")
	m.Assets.AnimatedDark = own("animated-dark", src.AnimatedDark)
	m.Assets.Assistant = own("assistant", src.Assistant.Avatar)
	m.ShortName, m.Description, m.StorageNamespace = src.ShortName, src.Description, src.StorageNamespace
	if len(src.Names) > 0 {
		m.Names = make(map[string]string, len(src.Names))
		for k, v := range src.Names {
			m.Names[k] = v
		}
	}
	if !src.Theme.isZero() {
		t := src.Theme
		if t.Primary == "" {
			t.Primary = src.Color
		}
		t.Gradient = append([]string(nil), t.Gradient...)
		if t.Dark != nil {
			d := *t.Dark
			t.Dark = &d
		}
		m.Theme = &t
	}
	if a := src.Assistant; a.Name != "" || a.Avatar.present() || a.Mascot != nil {
		am := &AssistantManifest{Name: a.Name, Avatar: m.Assets.Assistant}
		if a.Mascot != nil {
			c := *a.Mascot
			if c.Face != nil {
				f := *c.Face
				c.Face = &f
			}
			am.Mascot = &c
		}
		m.Assistant = am
	}
	if versioned {
		m.Version = version
	}
	return m
}

func asset(url string, f File) Asset {
	return Asset{URL: url, Type: f.mime(), Width: f.Width, Height: f.Height}
}
