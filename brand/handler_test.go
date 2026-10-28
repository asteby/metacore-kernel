package brand

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testSource() Source {
	return Source{
		Key:   "pitsline",
		Name:  "Pitsline",
		Color: "#EB2221",
		Icon:  File{Bytes: []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"/>`), Width: 128, Height: 128},
		Logo:  File{Bytes: []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 13 8"/>`), Width: 1300, Height: 800},
	}
}

func TestHandlerManifest(t *testing.T) {
	h := Handler(testSource())
	for _, path := range []string{"/api/brand", "/brand", "/api/brand/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s content-type=%q", path, ct)
		}
		var m Manifest
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s decode: %v", path, err)
		}
		if m.Spec != Spec || m.Key != "pitsline" || m.Name != "Pitsline" || m.Color != "#EB2221" {
			t.Fatalf("%s manifest=%+v", path, m)
		}
		if m.Assets.Icon.URL != "/api/brand/icon" || m.Assets.Logo == nil || m.Assets.Logo.URL != "/api/brand/logo" {
			t.Fatalf("%s assets=%+v", path, m.Assets)
		}
		if m.Assets.OG != nil {
			t.Fatalf("%s unexpected og: %+v", path, m.Assets.OG)
		}
	}
}

func TestHandlerAssets(t *testing.T) {
	src := testSource()
	h := Handler(src)
	cases := []struct {
		path string
		want []byte
	}{
		{"/api/brand/icon", src.Icon.Bytes},
		{"/brand/logo", src.Logo.Bytes},
		{"/api/brand/logo", src.Logo.Bytes},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", tc.path, w.Code)
		}
		if got, err := io.ReadAll(w.Body); err != nil || string(got) != string(tc.want) {
			t.Fatalf("%s body=%q want %q err=%v", tc.path, got, tc.want, err)
		}
		if ct := w.Header().Get("Content-Type"); ct != "image/svg+xml" {
			t.Fatalf("%s content-type=%q", tc.path, ct)
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("%s missing svg CSP", tc.path)
		}
	}
}

func TestHandlerLogoFallsBackToIcon(t *testing.T) {
	src := testSource()
	src.Logo = File{}
	h := Handler(src)
	req := httptest.NewRequest(http.MethodGet, "/api/brand/logo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Body.String() != string(src.Icon.Bytes) {
		t.Fatalf("logo fallback did not serve icon")
	}
}

func TestHandlerOGMissing(t *testing.T) {
	h := Handler(testSource())
	req := httptest.NewRequest(http.MethodGet, "/api/brand/og", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestHandlerUnknownPath(t *testing.T) {
	h := Handler(testSource())
	req := httptest.NewRequest(http.MethodGet, "/api/brand/nope", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func manifestOf(t *testing.T, h http.Handler, path string) Manifest {
	t.Helper()
	w := get(h, path)
	if w.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
	}
	var m Manifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// richSource exercises every optional slot.
func richSource() Source {
	s := testSource()
	s.LogoDark = File{Bytes: svg("0 0 13 8")}
	s.Favicon = File{Bytes: pngBytes(32, 32), Type: "image/png", Width: 32, Height: 32}
	s.Maskable = File{Bytes: pngBytes(512, 512), Type: "image/png", Width: 512, Height: 512}
	s.Monochrome = File{Bytes: svg("0 0 1 1")}
	s.Animated = File{Bytes: svg("0 0 1 1")}
	s.AnimatedDark = File{Bytes: svg("0 0 2 2")}
	s.Assistant = Assistant{Name: "Vanta", Avatar: File{Bytes: svg("0 0 4 4")}}
	s.ShortName, s.Description, s.StorageNamespace = "Pits", "desc", "pits"
	s.Theme = Theme{Accent: "#00fcff", Gradient: []string{"#00fcff", "#0033f0"}}
	return s
}

// The spec-1 output of Handler(src) must stay byte-identical (golden captured
// from the implementation before the extension).
func TestManifestSpec1Golden(t *testing.T) {
	src := testSource()
	src.OG = File{Bytes: []byte("PNGDATA"), Type: "image/png", Width: 1200, Height: 630}
	w := get(Handler(src), "/api/brand")
	want, err := os.ReadFile("testdata/manifest_v1.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if w.Body.String() != string(want) {
		t.Fatalf("spec-1 manifest drifted:\n got %s\nwant %s", w.Body.String(), want)
	}
	if w.Header().Get("Cache-Control") != "public, max-age=86400" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("headers = %v", w.Header())
	}
}

func TestNewRoutesAndFallbacks(t *testing.T) {
	src := richSource()
	h := Handler(src)
	for path, want := range map[string]string{
		"/api/brand/logo-dark":     string(src.LogoDark.Bytes),
		"/brand/favicon":           string(src.Favicon.Bytes),
		"/api/brand/maskable":      string(src.Maskable.Bytes),
		"/api/brand/monochrome":    string(src.Monochrome.Bytes),
		"/api/brand/animated":      string(src.Animated.Bytes),
		"/api/brand/animated-dark": string(src.AnimatedDark.Bytes),
		"/api/brand/assistant":     string(src.Assistant.Avatar.Bytes),
	} {
		w := get(h, path)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s status=%d", path, w.Code)
		}
	}
	if ct := get(h, "/api/brand/favicon").Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("favicon content-type %q", ct)
	}

	// fallbacks: logo-dark -> logo -> icon, favicon -> icon
	min := testSource()
	h = Handler(min)
	if get(h, "/api/brand/logo-dark").Body.String() != string(min.Logo.Bytes) {
		t.Error("logo-dark did not fall back to logo")
	}
	min.Logo = File{}
	h = Handler(min)
	if get(h, "/api/brand/logo-dark").Body.String() != string(min.Icon.Bytes) {
		t.Error("logo-dark did not fall back to icon")
	}
	if get(h, "/api/brand/favicon").Body.String() != string(min.Icon.Bytes) {
		t.Error("favicon did not fall back to icon")
	}
	for _, p := range []string{"maskable", "monochrome", "animated", "animated-dark", "assistant", "og"} {
		if c := get(h, "/api/brand/"+p).Code; c != http.StatusNotFound {
			t.Errorf("%s without a file: %d, want 404", p, c)
		}
	}
	for _, p := range []string{"/api/brand/logo-dark/x", "/api/brand/icon/logo", "/api/brand/ICON"} {
		if c := get(h, p).Code; c != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, c)
		}
	}
}

func TestManifestAdditiveFields(t *testing.T) {
	m := manifestOf(t, Handler(richSource()), "/api/brand")
	a := m.Assets
	if a.LogoDark == nil || a.LogoDark.URL != "/api/brand/logo-dark" || a.Favicon == nil || a.Favicon.Type != "image/png" ||
		a.Maskable == nil || a.Monochrome == nil || a.Animated == nil || a.AnimatedDark == nil || a.Assistant == nil {
		t.Fatalf("assets = %+v", a)
	}
	if m.ShortName != "Pits" || m.Description != "desc" || m.StorageNamespace != "pits" {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Theme == nil || m.Theme.Primary != "#EB2221" || m.Theme.Accent != "#00fcff" {
		t.Fatalf("theme primary should default to color: %+v", m.Theme)
	}
	if m.Assistant == nil || m.Assistant.Name != "Vanta" || m.Assistant.Avatar == nil || m.Assistant.Avatar.URL != "/api/brand/assistant" {
		t.Fatalf("assistant = %+v", m.Assistant)
	}
	if m.Version != "" || strings.Contains(m.Assets.Icon.URL, "?v=") {
		t.Fatal("version / ?v= must be opt-in")
	}
	// optional slots are omitted when absent
	if body := get(Handler(testSource()), "/api/brand").Body.String(); strings.Contains(body, "logo_dark") || strings.Contains(body, "theme") || strings.Contains(body, "assistant") {
		t.Fatalf("empty optional fields leaked: %s", body)
	}
	// color falls back to theme.primary
	s := testSource()
	s.Color, s.Theme.Primary = "", "#0033f0"
	if m := manifestOf(t, Handler(s), "/api/brand"); m.Color != "#0033f0" {
		t.Fatalf("color = %q", m.Color)
	}
}

func TestETagAnd304(t *testing.T) {
	h := Handler(richSource())
	for _, path := range []string{"/api/brand", "/api/brand/icon", "/api/brand/favicon"} {
		first := get(h, path)
		etag := first.Header().Get("ETag")
		if etag == "" || !strings.HasPrefix(etag, `"`) {
			t.Fatalf("%s etag=%q", path, etag)
		}
		for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			w := get(h, path, "If-None-Match", inm)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
				t.Errorf("%s If-None-Match %q: status=%d body=%d", path, inm, w.Code, w.Body.Len())
			}
			if w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") == "" {
				t.Errorf("%s 304 lacks validators: %v", path, w.Header())
			}
		}
		if w := get(h, path, "If-None-Match", `"stale"`); w.Code != http.StatusOK {
			t.Errorf("%s stale etag: %d", path, w.Code)
		}
		if again := get(h, path).Header().Get("ETag"); again != etag {
			t.Errorf("%s etag unstable: %q vs %q", path, etag, again)
		}
	}
}

func TestAssetsSandboxedAndCORS(t *testing.T) {
	h := Handler(richSource())
	w := get(h, "/api/brand/favicon") // PNG
	if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("png headers = %v", w.Header())
	}
}

func TestVersionedURLs(t *testing.T) {
	src := richSource()
	h := NewHandler(Static(src), WithVersionedURLs())
	m := manifestOf(t, h, "/api/brand")
	if len(m.Version) != 12 {
		t.Fatalf("version = %q", m.Version)
	}
	for _, u := range []string{m.Assets.Icon.URL, m.Assets.Logo.URL, m.Assets.LogoDark.URL, m.Assets.Favicon.URL, m.Assets.Assistant.URL, m.Assistant.Avatar.URL} {
		if !strings.HasSuffix(u, "?v="+m.Version) {
			t.Errorf("url %q lacks ?v=%s", u, m.Version)
		}
	}
	// the asset answers the same with or without ?v=, immutable only for the current version
	plain := get(h, "/api/brand/icon")
	cur := get(h, "/api/brand/icon?v="+m.Version)
	stale := get(h, "/api/brand/icon?v=000000000000")
	if plain.Body.String() != cur.Body.String() || plain.Code != 200 || cur.Code != 200 {
		t.Fatal("?v= changed the response")
	}
	if cc := cur.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("current ?v= cache-control = %q", cc)
	}
	if strings.Contains(stale.Header().Get("Cache-Control"), "immutable") || strings.Contains(plain.Header().Get("Cache-Control"), "immutable") {
		t.Error("immutable without the current version")
	}
}

type mutableProvider struct{ src Source }

func (p *mutableProvider) Brand() Source { return p.src }

func TestDynamicProviderChangesETag(t *testing.T) {
	p := &mutableProvider{src: richSource()}
	h := NewHandler(p, WithVersionedURLs())
	before := get(h, "/api/brand")
	icon0 := get(h, "/api/brand/icon").Header().Get("ETag")

	p.src.Icon = File{Bytes: svg("0 0 9 9")}
	after := get(h, "/api/brand")
	if before.Header().Get("ETag") == after.Header().Get("ETag") {
		t.Fatal("manifest ETag did not change with the brand")
	}
	if icon0 == get(h, "/api/brand/icon").Header().Get("ETag") {
		t.Fatal("icon ETag did not change with the brand")
	}
	if w := get(h, "/api/brand", "If-None-Match", before.Header().Get("ETag")); w.Code != http.StatusOK {
		t.Fatalf("old ETag must not 304 after a change: %d", w.Code)
	}
	var m0, m1 Manifest
	_ = json.Unmarshal(before.Body.Bytes(), &m0)
	_ = json.Unmarshal(after.Body.Bytes(), &m1)
	if m0.Version == m1.Version {
		t.Fatal("version unchanged")
	}
}

func TestWithVersionCachesSnapshot(t *testing.T) {
	calls := 0
	p := &countingProvider{src: richSource(), n: &calls}
	token := "a"
	h := NewHandler(p, WithVersion(func() string { return token }))
	for i := 0; i < 5; i++ {
		get(h, "/api/brand")
		get(h, "/api/brand/icon")
	}
	if calls != 1 {
		t.Fatalf("Brand() called %d times for an unchanged token, want 1", calls)
	}
	e1 := get(h, "/api/brand").Header().Get("ETag")
	token = "b"
	get(h, "/api/brand")
	if calls != 2 {
		t.Fatalf("Brand() calls after token change = %d, want 2", calls)
	}
	_ = e1
}

type countingProvider struct {
	src Source
	n   *int
}

func (p *countingProvider) Brand() Source { *p.n++; return p.src }

func TestWithMaxAge(t *testing.T) {
	h := NewHandler(Static(testSource()), WithMaxAge(5*time.Minute, 60*time.Second))
	if cc := get(h, "/api/brand").Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("manifest cache-control %q", cc)
	}
	if cc := get(h, "/api/brand/icon").Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Errorf("asset cache-control %q", cc)
	}
	if cc := get(Handler(testSource()), "/api/brand/icon").Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("default cache-control %q", cc)
	}
}

func TestHeadAndMethods(t *testing.T) {
	h := Handler(testSource())
	req := httptest.NewRequest(http.MethodHead, "/api/brand/icon", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("ETag") == "" {
		t.Fatalf("HEAD status=%d body=%d", w.Code, w.Body.Len())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/brand", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", w.Code)
	}
}

func TestManifestWithoutIcon(t *testing.T) {
	if c := get(Handler(Source{Key: "x"}), "/api/brand").Code; c != http.StatusNotFound {
		t.Fatalf("status=%d", c)
	}
}
