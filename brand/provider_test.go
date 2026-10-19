package brand

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func svg(s string) File {
	return File{Bytes: []byte(`<svg xmlns="http://www.w3.org/2000/svg">` + s + `</svg>`)}
}

func TestCascade_FieldByField(t *testing.T) {
	product := Layer{Source: "product", Key: "ops", Name: "Ops", Color: "#000000", Icon: svg("p")}
	instance := Layer{Source: "instance", Name: "Acme", Color: "#112233", Version: 3}
	r := Cascade(instance, product)
	if r.Key != "ops" || r.Name != "Acme" || r.Color != "#112233" || string(r.Icon.Bytes) != string(svg("p").Bytes) {
		t.Fatalf("resolved = %+v", r)
	}
	if r.Source != "instance" || r.Version != 3 || r.Locked {
		t.Fatalf("source/version/locked = %q %d %v", r.Source, r.Version, r.Locked)
	}
	// Empty layers are ignored.
	if r := Cascade(Layer{}, product); r.Source != "product" || r.Name != "Ops" {
		t.Fatalf("empty layer: %+v", r)
	}
}

func TestCascade_Locked(t *testing.T) {
	product := Layer{Source: "product", Name: "Ops", Icon: svg("p")}
	tenant := Layer{Source: "org", Name: "Tienda X", Color: "#ffffff"}

	unlocked := Layer{Source: "instance", Name: "Acme"}
	if r := Cascade(tenant, unlocked, product); r.Name != "Tienda X" || r.Source != "org" || r.Locked {
		t.Fatalf("unlocked: tenant must win, got %+v", r)
	}
	locked := Layer{Source: "instance", Name: "Acme", Locked: true}
	r := Cascade(tenant, locked, product)
	if r.Name != "Acme" || r.Color != "" || r.Source != "instance" || !r.Locked {
		t.Fatalf("locked: tenant must be ignored, got %+v", r)
	}
}

func TestLayerFromPack_ArtworkFallsBackWithinPack(t *testing.T) {
	logo := []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)
	p := &Pack{Name: "Acme", PrimaryColor: "#112233", Locked: true, Version: 7,
		Assets: PackAssets{Logo: &PackAsset{Type: TypeSVG, Bytes: logo}}}
	l := LayerFromPack(p)
	for name, f := range map[string]File{"icon": l.Icon, "logo": l.Logo, "favicon": l.Favicon} {
		if !bytes.Equal(f.Bytes, logo) {
			t.Errorf("%s must fall back to the pack logo", name)
		}
	}
	if !l.Locked || l.Version != 7 || l.Source != "instance" {
		t.Fatalf("layer = %+v", l)
	}
	if !LayerFromPack(nil).empty() {
		t.Fatal("nil pack must give an empty layer")
	}
}

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHandlerFrom_NoOverrideMatchesHandler(t *testing.T) {
	src := testSource()
	for _, p := range []Provider{nil, ProviderFunc(func() []Layer { return nil }), ProviderFunc(func() []Layer { return []Layer{{}} })} {
		dyn, static := HandlerFrom(p, src), Handler(src)
		for _, path := range []string{"/api/brand", "/api/brand/icon", "/api/brand/logo", "/api/brand/og"} {
			a, b := get(t, dyn, path, nil), get(t, static, path, nil)
			if a.Code != b.Code || a.Body.String() != b.Body.String() ||
				a.Header().Get("Cache-Control") != b.Header().Get("Cache-Control") || a.Header().Get("ETag") != "" {
				t.Fatalf("%s: dynamic differs from Handler: %d %q vs %d %q", path, a.Code, a.Header(), b.Code, b.Header())
			}
		}
		if w := get(t, dyn, "/api/brand/favicon", nil); w.Code != 200 || w.Body.String() != string(src.Icon.Bytes) {
			t.Fatalf("favicon without override must be the product icon: %d", w.Code)
		}
	}
}

func TestHandlerFrom_OverrideIsDynamicWithETag(t *testing.T) {
	src := testSource()
	var cur atomic.Pointer[Layer]
	h := HandlerFrom(ProviderFunc(func() []Layer {
		if l := cur.Load(); l != nil {
			return []Layer{*l}
		}
		return nil
	}), src)

	if w := get(t, h, "/api/brand", nil); w.Header().Get("ETag") != "" {
		t.Fatal("no override: no ETag")
	}

	icon := svg(`<circle r="1"/>`)
	cur.Store(&Layer{Source: "instance", Name: "Acme", Color: "#112233", AccentColor: "#445566", Icon: icon, Locked: true, Version: 3})
	w := get(t, h, "/api/brand", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("override manifest: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	tag := w.Header().Get("ETag")
	if tag == "" {
		t.Fatal("override manifest needs an ETag")
	}
	var m Manifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Key != "pitsline" || m.Name != "Acme" || m.AccentColor != "#445566" || m.Source != "instance" || m.Version != 3 || !m.Locked {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Assets.Logo == nil || m.Assets.Logo.URL != "/api/brand/logo" {
		t.Fatalf("logo asset = %+v", m.Assets.Logo)
	}
	if w := get(t, h, "/api/brand", map[string]string{"If-None-Match": tag}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d", w.Code)
	}
	if w := get(t, h, "/api/brand/icon", nil); w.Body.String() != string(icon.Bytes) || w.Header().Get("ETag") == "" ||
		w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("override icon: %q %q", w.Body.String(), w.Header())
	}
	// Logo: the instance layer has none, so the product logo shows through.
	if w := get(t, h, "/api/brand/logo", nil); w.Body.String() != string(src.Logo.Bytes) {
		t.Fatalf("logo should cascade to product: %q", w.Body.String())
	}

	// A new pack changes the response without rebuilding the handler.
	cur.Store(&Layer{Source: "instance", Name: "Acme 2"})
	w = get(t, h, "/api/brand", map[string]string{"If-None-Match": tag})
	if w.Code != 200 || w.Header().Get("ETag") == tag {
		t.Fatalf("new content must change ETag: %d", w.Code)
	}
	cur.Store(nil)
	if w := get(t, h, "/api/brand", nil); w.Header().Get("Cache-Control") != cacheControl {
		t.Fatal("removing the override must restore the product handler")
	}
}
