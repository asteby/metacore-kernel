package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestFromV3MapsDesktopClients(t *testing.T) {
	raw := []byte(`{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {"key": "demo", "name": "Demo", "version": "0.1.0", "description": "d"},
  "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
  "tenancy": {"isolation": "shared", "rls_column": "organization_id"},
  "desktop_clients": [{
    "key": "agent",
    "label": "Agent",
    "auth": "ops_user",
    "download_url": "https://hub.asteby.com/downloads/demo",
    "brand": "#abc",
    "files": {"linux": "demo.deb"}
  }]
}`)
	v3m, err := v3.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := manifest.FromV3(v3m)
	if len(host.DesktopClients) != 1 {
		t.Fatalf("len=%d", len(host.DesktopClients))
	}
	if host.DesktopClients[0].DownloadURL != "https://hub.asteby.com/downloads/demo" {
		t.Fatalf("%+v", host.DesktopClients[0])
	}
	if host.DesktopClients[0].Files == nil || host.DesktopClients[0].Files.Linux != "demo.deb" {
		t.Fatalf("files %+v", host.DesktopClients[0].Files)
	}
}
