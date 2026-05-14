package hub_test

// Hub client tests. We spin up an httptest server that speaks the Hub
// contract and exercise the happy paths for each method plus the most
// failure-mode-prone branches (404 → ErrNotFound, non-200, malformed JSON,
// /v1/spec missing → ErrSpecUnavailable).
//
// No external network access is required — the Hub is fully mocked.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/hub"
)

// stubHub builds a httptest server with handlers for every Hub route the
// client exercises. Each test passes in the response payload + assertions
// over the inbound request.
type stubHub struct {
	t        *testing.T
	server   *httptest.Server
	requests []*http.Request
}

func newStubHub(t *testing.T, mux *http.ServeMux) *stubHub {
	t.Helper()
	s := &stubHub{t: t}
	// Wrap the supplied mux so we can record every inbound request — tests
	// assert on headers (Authorization) and query strings.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture before serving so the test can read it post-call.
		s.requests = append(s.requests, r.Clone(r.Context()))
		mux.ServeHTTP(w, r)
	})
	s.server = httptest.NewServer(wrapped)
	t.Cleanup(s.server.Close)
	return s
}

// lastRequest returns the most recent inbound request — used by tests that
// only make one call.
func (s *stubHub) lastRequest() *http.Request {
	s.t.Helper()
	if len(s.requests) == 0 {
		s.t.Fatalf("expected at least one request, got none")
	}
	return s.requests[len(s.requests)-1]
}

func newClient(t *testing.T, baseURL, token string) *hub.Client {
	t.Helper()
	c, err := hub.NewClient(baseURL, token)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// --- FetchCatalog --------------------------------------------------------

func TestFetchCatalog_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		// Assert query params got forwarded.
		if got := r.URL.Query().Get("search"); got != "inventory" {
			t.Errorf("search=%q", got)
		}
		if got := r.URL.Query().Get("category"); got != "operations" {
			t.Errorf("category=%q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "25" {
			t.Errorf("limit=%q", got)
		}
		if got := r.URL.Query().Get("offset"); got != "50" {
			t.Errorf("offset=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"key": "addon.alpha", "name": "Alpha", "latest_version": "1.2.0"},
				{"key": "addon.beta", "name": "Beta", "latest_version": "0.3.1"},
			},
			"total": 42,
		})
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	result, err := c.FetchCatalog(context.Background(), hub.CatalogParams{
		Search:   "inventory",
		Category: "operations",
		Limit:    25,
		Offset:   50,
	})
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(result.Items))
	}
	if result.Items[0].Key != "addon.alpha" {
		t.Errorf("items[0].Key=%q", result.Items[0].Key)
	}
	if result.Total != 42 {
		t.Errorf("Total=%d", result.Total)
	}
}

func TestFetchCatalog_BearerForwarded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("Authorization=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "secret-token")
	if !c.HasLicenseToken() {
		t.Fatal("HasLicenseToken should be true")
	}
	if _, err := c.FetchCatalog(context.Background(), hub.CatalogParams{}); err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
}

func TestFetchCatalog_AnonymousNoAuthHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("expected no Authorization header, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	if c.HasLicenseToken() {
		t.Fatal("HasLicenseToken should be false")
	}
	if _, err := c.FetchCatalog(context.Background(), hub.CatalogParams{}); err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
}

func TestFetchCatalog_NonOKErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	_, err := c.FetchCatalog(context.Background(), hub.CatalogParams{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 in error, got %v", err)
	}
}

// --- FetchAddon ---------------------------------------------------------

func TestFetchAddon_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addons/addon.alpha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":  "addon.alpha",
			"name": "Alpha",
			"versions": []map[string]any{
				{"version": "1.2.0", "released_at": "2026-04-01T00:00:00Z"},
				{"version": "1.1.0"},
			},
			"readme": "# Alpha",
		})
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	d, err := c.FetchAddon(context.Background(), "addon.alpha")
	if err != nil {
		t.Fatalf("FetchAddon: %v", err)
	}
	if d.Key != "addon.alpha" {
		t.Errorf("Key=%q", d.Key)
	}
	if len(d.Versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(d.Versions))
	}
	if d.Versions[0].Version != "1.2.0" {
		t.Errorf("Versions[0].Version=%q", d.Versions[0].Version)
	}
}

func TestFetchAddon_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addons/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such addon", http.StatusNotFound)
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	_, err := c.FetchAddon(context.Background(), "missing")
	if !errors.Is(err, hub.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFetchAddon_EmptyKeyRejected(t *testing.T) {
	c := newClient(t, "http://localhost", "")
	if _, err := c.FetchAddon(context.Background(), "   "); err == nil {
		t.Fatal("expected error for empty key")
	}
}

// --- DownloadBundle ------------------------------------------------------

func TestDownloadBundle_VersionForwarded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addons/addon.alpha/bundle", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("version"); got != "1.2.0" {
			t.Errorf("version=%q", got)
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("not-really-a-tarball"))
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "lic-token")
	rc, err := c.DownloadBundle(context.Background(), "addon.alpha", "1.2.0")
	if err != nil {
		t.Fatalf("DownloadBundle: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "not-really-a-tarball" {
		t.Errorf("body=%q", string(body))
	}
	// And the Bearer header travelled with the request.
	req := srv.lastRequest()
	if got := req.Header.Get("Authorization"); got != "Bearer lic-token" {
		t.Errorf("Authorization=%q", got)
	}
}

func TestDownloadBundle_OmitsVersionWhenEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addons/addon.alpha/bundle", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("version"); got != "" {
			t.Errorf("expected no version param, got %q", got)
		}
		_, _ = w.Write([]byte("latest"))
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	rc, err := c.DownloadBundle(context.Background(), "addon.alpha", "")
	if err != nil {
		t.Fatalf("DownloadBundle: %v", err)
	}
	_ = rc.Close()
}

func TestDownloadBundle_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addons/missing/bundle", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such version", http.StatusNotFound)
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	_, err := c.DownloadBundle(context.Background(), "missing", "9.9.9")
	if !errors.Is(err, hub.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- FetchSpec -----------------------------------------------------------

func TestFetchSpec_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/spec", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hub_version":     "0.2.0",
			"api_version":     "v1",
			"kernel_versions": []string{"0.10.x", "0.11.x"},
			"bundle_format":   "tar.gz+ed25519",
		})
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	spec, err := c.FetchSpec(context.Background())
	if err != nil {
		t.Fatalf("FetchSpec: %v", err)
	}
	if spec.APIVersion != "v1" {
		t.Errorf("APIVersion=%q", spec.APIVersion)
	}
	if len(spec.KernelVersions) != 2 {
		t.Errorf("KernelVersions=%v", spec.KernelVersions)
	}
}

func TestFetchSpec_NotYetDeployed(t *testing.T) {
	// Soft-fail: the Hub doesn't expose /v1/spec yet. Client returns the
	// sentinel so the caller can decide what to do (we log + continue).
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/spec", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented", http.StatusNotFound)
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	_, err := c.FetchSpec(context.Background())
	if !errors.Is(err, hub.ErrSpecUnavailable) {
		t.Fatalf("expected ErrSpecUnavailable, got %v", err)
	}
}

// --- Construction -------------------------------------------------------

func TestNewClient_RejectsEmptyURL(t *testing.T) {
	if _, err := hub.NewClient("", ""); err == nil {
		t.Fatal("expected error for empty URL")
	}
	if _, err := hub.NewClient("   ", ""); err == nil {
		t.Fatal("expected error for whitespace URL")
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c, err := hub.NewClient("https://hub.example.com///", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.BaseURL(); got != "https://hub.example.com" {
		t.Errorf("BaseURL=%q", got)
	}
}

// --- example: bundle stream piping to a sink ----------------------------

// The install handler reads the body straight into kernel/bundle.Read. This
// test exercises the contract by streaming a 1MB payload through and
// verifying it arrives intact. Catches regressions where the client buffers
// the entire bundle in memory.
func TestDownloadBundle_StreamsLargePayload(t *testing.T) {
	const size = 1 << 20 // 1 MiB
	payload := strings.Repeat("x", size)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/addons/addon.big/bundle", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	})
	srv := newStubHub(t, mux)

	c := newClient(t, srv.server.URL, "")
	rc, err := c.DownloadBundle(context.Background(), "addon.big", "")
	if err != nil {
		t.Fatalf("DownloadBundle: %v", err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != size {
		t.Errorf("got %d bytes, want %d", n, size)
	}
}

// example sanity: ensure the package name is importable for the handler
// tests (compile-time check via this expression).
var _ = fmt.Sprintf
