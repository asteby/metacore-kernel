package brand

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	cacheControl = "public, max-age=86400"
	svgCSP       = "sandbox; default-src 'none'; style-src 'unsafe-inline'"
)

// Handler serves the brand manifest and its artwork. It matches on the
// path suffix so the same handler works mounted at /brand (chi behind an
// /api/ strip) or /api/brand (Fiber group).
func Handler(src Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch kind(r.URL.Path) {
		case "manifest":
			writeManifest(w, src)
		case "icon":
			writeFile(w, src.Icon)
		case "logo":
			writeFile(w, src.logo())
		case "og":
			writeFile(w, src.OG)
		default:
			http.NotFound(w, r)
		}
	})
}

func kind(path string) string {
	p := strings.TrimSuffix(path, "/")
	switch {
	case strings.HasSuffix(p, "/brand/icon"):
		return "icon"
	case strings.HasSuffix(p, "/brand/logo"):
		return "logo"
	case strings.HasSuffix(p, "/brand/og"):
		return "og"
	case strings.HasSuffix(p, "/brand"):
		return "manifest"
	default:
		return ""
	}
}

func writeManifest(w http.ResponseWriter, src Source) {
	if !src.Icon.present() {
		http.Error(w, "brand icon not configured", http.StatusNotFound)
		return
	}
	m := Manifest{Spec: Spec, Key: src.Key, Name: src.Name, Color: src.Color}
	m.Assets.Icon = asset(DefaultPrefix+"/icon", src.Icon)
	if src.Logo.present() {
		a := asset(DefaultPrefix+"/logo", src.Logo)
		m.Assets.Logo = &a
	} else {
		a := asset(DefaultPrefix+"/logo", src.Icon)
		m.Assets.Logo = &a
	}
	if src.OG.present() {
		a := asset(DefaultPrefix+"/og", src.OG)
		m.Assets.OG = &a
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(m)
}

func asset(url string, f File) Asset {
	return Asset{URL: url, Type: f.mime(), Width: f.Width, Height: f.Height}
}

func writeFile(w http.ResponseWriter, f File) {
	if !f.present() {
		http.Error(w, "brand asset not found", http.StatusNotFound)
		return
	}
	mime := f.mime()
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", cacheControl)
	if strings.HasPrefix(mime, "image/svg") {
		w.Header().Set("Content-Security-Policy", svgCSP)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f.Bytes)
}
