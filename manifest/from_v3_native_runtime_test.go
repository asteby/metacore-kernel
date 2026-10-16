package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestFromV3ProjectsNativeServiceBackend(t *testing.T) {
	raw := []byte(`{
  "apiVersion":"asteby.com/v3", "kind":"Addon",
  "metadata":{"key":"connector_whatsapp","name":"WhatsApp","version":"1.0.0"},
  "compatibility":{"requires":[{"key":"kernel","version":">=3.0.0 <4.0.0"}]},
  "runtime":{"native_service":{
    "entrypoint":"backend/connector", "scope":"instance",
    "health":{"path":"/health","interval":"10s","timeout":"2s"},
    "resources":{"memory_mb":512,"cpu_quota_mcpu":500,"pids":128},
    "network":{"egress":["web.whatsapp.com:443"]},
    "artifacts":[{"os":"linux","arch":"amd64","path":"backend/native/linux-amd64.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sbom":"backend/native/linux-amd64.spdx.json"}],
    "control":{"protocol":"metacore.native/v1","transport":"unix_http"}
  }}
}`)
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := manifest.FromV3(m)
	if out.NativeService == nil {
		t.Fatalf("native service was not projected: %#v", out.NativeService)
	}
	if out.NativeService.Entrypoint != "backend/connector" || out.NativeService.Scope != "instance" {
		t.Fatalf("native service projection lost fields: %#v", out.NativeService)
	}
}

func TestFromV3ProjectsNativeServiceIngress(t *testing.T) {
	raw := []byte(`{
  "apiVersion":"asteby.com/v3", "kind":"Addon",
  "metadata":{"key":"storefront","name":"Tienda en línea","version":"1.0.0"},
  "compatibility":{"requires":[{"key":"kernel","version":">=3.0.0 <4.0.0"}]},
  "runtime":{"native_service":{
    "entrypoint":"backend/renderer", "scope":"instance",
    "health":{"path":"/healthz"},
    "resources":{"memory_mb":512,"cpu_quota_mcpu":1000,"pids":256},
    "network":{},
    "artifacts":[{"os":"linux","arch":"amd64","path":"backend/native/renderer-linux-amd64.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sbom":"backend/native/renderer.spdx.json","download_url":"https://github.com/asteby-hq/local/releases/download/v1.0.0/renderer-linux-amd64.tar.gz"}],
    "control":{"protocol":"metacore.native/v1","transport":"unix_http"},
    "ingress":{"kind":"http","timeout":"20s"}
  }}
}`)
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := manifest.FromV3(m)
	if out.NativeService == nil || out.NativeService.Ingress == nil || out.NativeService.Ingress.Kind != "http" || out.NativeService.Ingress.Timeout != "20s" {
		t.Fatalf("ingress was not projected: %#v", out.NativeService)
	}
	if out.NativeService.Ingress == m.Runtime.NativeService.Ingress {
		t.Fatal("projection must not alias the v3 ingress pointer")
	}
	if err := out.NativeService.Validate(); err != nil {
		t.Fatalf("projected spec invalid: %v", err)
	}
}
