package v3

import (
	"strings"
	"testing"
)

func nativeRuntime() map[string]interface{} {
	return map[string]interface{}{
		"native_service": map[string]interface{}{
			"entrypoint": "backend/connector",
			"args":       []interface{}{"serve"},
			"scope":      "instance",
			"health": map[string]interface{}{
				"path": "/health", "interval": "10s", "timeout": "2s",
			},
			"resources": map[string]interface{}{
				"memory_mb": 512, "cpu_quota_mcpu": 500, "pids": 128,
			},
			"network": map[string]interface{}{
				"egress": []interface{}{"web.whatsapp.com:443"},
			},
			"secrets": []interface{}{map[string]interface{}{
				"handle": "whatsapp.session", "mount": "/run/secrets/session",
			}},
			"artifacts": []interface{}{map[string]interface{}{
				"os": "linux", "arch": "amd64", "path": "backend/native/linux-amd64.tar.gz",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"sbom":   "backend/native/linux-amd64.spdx.json",
			}},
		},
	}
}

func TestValidateNativeService(t *testing.T) {
	m := baseValid()
	m["runtime"] = nativeRuntime()
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid native runtime: %v", err)
	}
}

func TestValidateNativeServiceRejectsTraversal(t *testing.T) {
	m := baseValid()
	runtime := nativeRuntime()
	runtime["native_service"].(map[string]interface{})["entrypoint"] = "../connector"
	m["runtime"] = runtime
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("expected entrypoint validation error, got %v", err)
	}
}

func TestValidateNativeServiceRejectsPreset(t *testing.T) {
	m := baseValid()
	m["kind"] = "Preset"
	m["preset"] = map[string]interface{}{"addons": []interface{}{map[string]interface{}{
		"key": "base", "version": ">=1.0.0",
	}}}
	m["runtime"] = nativeRuntime()
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "only valid for kind=Addon") {
		t.Fatalf("expected kind validation error, got %v", err)
	}
}
