package v3_test

import (
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestDesktopClientsRoundTrip(t *testing.T) {
	raw := []byte(`{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {
    "key": "demo_client",
    "name": "Demo",
    "version": "0.1.0",
    "description": "Desktop client declaration smoke test."
  },
  "compatibility": {
    "requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]
  },
  "tenancy": {"isolation": "shared", "rls_column": "organization_id"},
  "desktop_clients": [{
    "key": "agent",
    "label": "Demo Agent",
    "auth": "ops_user",
    "download_url": "https://hub.asteby.com/downloads/demo",
    "list_on_hub": false,
    "brand": "#0EA5E9",
    "logo": "/demo.svg",
    "files": {
      "mac": "demo.dmg",
      "windows": "demo-setup.msi",
      "linux": "demo-linux-amd64.deb"
    }
  }]
}`)
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.DesktopClients) != 1 {
		t.Fatalf("DesktopClients len=%d", len(m.DesktopClients))
	}
	c := m.DesktopClients[0]
	if c.Key != "agent" || c.Auth != "ops_user" || !strings.HasPrefix(c.DownloadURL, "https://") {
		t.Fatalf("unexpected client: %+v", c)
	}
	if c.Files == nil || c.Files.Linux != "demo-linux-amd64.deb" {
		t.Fatalf("files: %+v", c.Files)
	}
}

func TestDesktopClientsRejectHTTPAndBadAuth(t *testing.T) {
	base := `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {"key": "x", "name": "X", "version": "0.1.0", "description": "x"},
  "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
  "tenancy": {"isolation": "shared", "rls_column": "organization_id"},
  "desktop_clients": [%s]
}`
	cases := []string{
		`{"key":"a","auth":"edge_pair","download_url":"https://hub.asteby.com/d"}`,
		`{"key":"a","auth":"ops_user","download_url":"http://hub.asteby.com/d"}`,
		`{"key":"a","auth":"ops_user","download_url":"https://hub.asteby.com/d","files":{"mac":"../evil.dmg"}}`,
	}
	for _, body := range cases {
		_, err := v3.Parse([]byte(strings.Replace(base, "%s", body, 1)))
		if err == nil {
			t.Fatalf("expected error for %s", body)
		}
	}
}
