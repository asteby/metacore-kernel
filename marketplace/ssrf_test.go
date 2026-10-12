package marketplace_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/marketplace"
	"github.com/gofiber/fiber/v3"
)

// installWithURL posts an install for bundleURL against a handler built with
// opts and returns the HTTP status and the persisted error message.
func installWithURL(t *testing.T, bundleURL string, opts ...marketplace.HandlerOption) (int, string) {
	t.Helper()
	db := setupDB(t)
	orig := os.Getenv("ALLOW_UNSIGNED_BUNDLES")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", "true")
	inst := installer.New(db, "test")
	os.Setenv("ALLOW_UNSIGNED_BUNDLES", orig)

	h, err := marketplace.NewHandler(db, append([]marketplace.HandlerOption{marketplace.WithInstaller(inst)}, opts...)...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	app := fiber.New()
	h.Mount(app, func(c fiber.Ctx) error {
		c.Locals(auth.LocalOrganizationID, testOrgID)
		c.Locals(auth.LocalUserID, testUserID)
		return c.Next()
	})
	resp, err := app.Test(makeInstallReq(map[string]any{
		"addonKey": "com.example.addon", "version": "1.0.0", "bundleURL": bundleURL,
	}))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body := readBody(t, resp)
	msg := ""
	if d, ok := body["data"].(map[string]any); ok {
		msg, _ = d["error_message"].(string)
	}
	return resp.StatusCode, msg
}

func TestFetchBundle_RejectsUnsafeURLs(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", 404)
	}))
	defer srv.Close()

	cases := []struct {
		name, url string
		opts      []marketplace.HandlerOption
		want      string
	}{
		{"http non-loopback", "http://hub.example.com/b.tar.gz", nil, "https"},
		{"file scheme", "file:///etc/passwd", nil, "not allowed"},
		{"gopher scheme", "gopher://hub.example.com/", nil, "not allowed"},
		{"cloud metadata over http", "http://169.254.169.254/latest/meta-data", nil, "https"},
		{"userinfo", "https://user:pw@hub.example.com/b.tar.gz", nil, "credentials"},
		{"no host", "https:///b.tar.gz", nil, "host"},
		{"host outside hub origin", "https://evil.example.net/b.tar.gz",
			[]marketplace.HandlerOption{marketplace.WithHubURL("https://hub.example.com")}, "not an allowed"},
		{"look-alike host", "https://hub.example.com.evil.net/b.tar.gz",
			[]marketplace.HandlerOption{marketplace.WithHubURL("https://hub.example.com")}, "not an allowed"},
		{"internal host not in allowlist", "https://10.0.0.5/b.tar.gz",
			[]marketplace.HandlerOption{marketplace.WithBundleHostAllowlist("cdn.example.com")}, "not an allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := installWithURL(t, tc.url, tc.opts...)
			if code != 422 {
				t.Fatalf("status = %d, want 422", code)
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not contain %q", msg, tc.want)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("rejected URLs must not trigger requests, got %d", hits.Load())
	}
}

func TestFetchBundle_AllowsLoopbackHTTPAndAllowlisted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", 404)
	}))
	defer srv.Close()

	// Loopback http passes the guard (reaches the server, which 404s) even
	// with a Hub origin configured.
	code, msg := installWithURL(t, srv.URL+"/b.tar.gz", marketplace.WithHubURL("https://hub.example.com"))
	if code != 422 || !strings.Contains(msg, "404") {
		t.Fatalf("want download 404 failure, got %d %q", code, msg)
	}
	if hits.Load() != 1 {
		t.Fatalf("loopback request not made, hits=%d", hits.Load())
	}
}

func TestFetchBundle_BlocksRedirectToDisallowedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()
	code, msg := installWithURL(t, srv.URL+"/b.tar.gz")
	if code != 422 || !strings.Contains(msg, "redirect blocked") {
		t.Fatalf("want redirect blocked, got %d %q", code, msg)
	}
}
