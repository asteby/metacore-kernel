package brand

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
