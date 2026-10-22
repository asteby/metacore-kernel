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
			"control": map[string]interface{}{"protocol": "metacore.native/v1", "transport": "unix_http"},
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

func TestValidateNativeServiceIngress(t *testing.T) {
	for _, scope := range []string{"instance", "installation"} {
		m := baseValid()
		runtime := nativeRuntime()
		ns := runtime["native_service"].(map[string]interface{})
		ns["scope"] = scope
		ns["ingress"] = map[string]interface{}{"kind": "http", "path_prefix": "/", "max_body_bytes": 1048576, "timeout": "30s"}
		m["runtime"] = runtime
		if err := Validate(mustJSON(t, m)); err != nil {
			t.Fatalf("scope %s: expected valid ingress: %v", scope, err)
		}
	}
}

func TestValidateNativeServiceIngressRejects(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"kind":        {"kind": "tcp"},
		"path_prefix": {"kind": "http", "path_prefix": "/a/../b"},
		"timeout":     {"kind": "http", "timeout": "1h"},
		"unknown":     {"kind": "http", "hosts": []interface{}{"evil.example"}},
	}
	for name, ingress := range cases {
		m := baseValid()
		runtime := nativeRuntime()
		runtime["native_service"].(map[string]interface{})["ingress"] = ingress
		m["runtime"] = runtime
		if err := Validate(mustJSON(t, m)); err == nil {
			t.Errorf("%s: expected ingress validation error", name)
		}
	}
}

func TestValidateNativeServiceTCPListenersAndEgress(t *testing.T) {
	mk := func(mut func(ns map[string]interface{})) []byte {
		m := baseValid()
		runtime := nativeRuntime()
		mut(runtime["native_service"].(map[string]interface{}))
		m["runtime"] = runtime
		return mustJSON(t, m)
	}
	ok := mk(func(ns map[string]interface{}) {
		ns["network"] = map[string]interface{}{"egress": []interface{}{"*:25", "smtp.relay.example:587"}}
		ns["ingress"] = map[string]interface{}{"kind": "tcp", "listeners": []interface{}{
			map[string]interface{}{"name": "smtp", "port": 25, "proxy_protocol": "v2"},
			map[string]interface{}{"name": "submission", "port": 587},
		}}
	})
	if err := Validate(ok); err != nil {
		t.Fatalf("expected valid tcp ingress + port wildcard egress: %v", err)
	}
	// http ingress may carry listeners too.
	if err := Validate(mk(func(ns map[string]interface{}) {
		ns["ingress"] = map[string]interface{}{"kind": "http", "listeners": []interface{}{map[string]interface{}{"name": "smtp", "port": 25}}}
	})); err != nil {
		t.Fatalf("http+listeners: %v", err)
	}
	bad := map[string]func(ns map[string]interface{}){
		"tcp without listeners": func(ns map[string]interface{}) { ns["ingress"] = map[string]interface{}{"kind": "tcp"} },
		"tcp with path_prefix": func(ns map[string]interface{}) {
			ns["ingress"] = map[string]interface{}{"kind": "tcp", "path_prefix": "/x", "listeners": []interface{}{map[string]interface{}{"name": "a", "port": 25}}}
		},
		"port zero": func(ns map[string]interface{}) {
			ns["ingress"] = map[string]interface{}{"kind": "tcp", "listeners": []interface{}{map[string]interface{}{"name": "a", "port": 0}}}
		},
		"bad proxy_protocol": func(ns map[string]interface{}) {
			ns["ingress"] = map[string]interface{}{"kind": "tcp", "listeners": []interface{}{map[string]interface{}{"name": "a", "port": 25, "proxy_protocol": "v3"}}}
		},
		"duplicate port": func(ns map[string]interface{}) {
			ns["ingress"] = map[string]interface{}{"kind": "tcp", "listeners": []interface{}{
				map[string]interface{}{"name": "a", "port": 25}, map[string]interface{}{"name": "b", "port": 25}}}
		},
		"unknown listener field": func(ns map[string]interface{}) {
			ns["ingress"] = map[string]interface{}{"kind": "tcp", "listeners": []interface{}{map[string]interface{}{"name": "a", "port": 25, "bind": "0.0.0.0"}}}
		},
		"bare wildcard egress": func(ns map[string]interface{}) { ns["network"] = map[string]interface{}{"egress": []interface{}{"*"}} },
		"wildcard on web port": func(ns map[string]interface{}) {
			ns["network"] = map[string]interface{}{"egress": []interface{}{"*:443"}}
		},
		"subdomain wildcard port": func(ns map[string]interface{}) {
			ns["network"] = map[string]interface{}{"egress": []interface{}{"*.example.com:25"}}
		},
		"bad port": func(ns map[string]interface{}) {
			ns["network"] = map[string]interface{}{"egress": []interface{}{"host:99999"}}
		},
	}
	for name, mut := range bad {
		if err := Validate(mk(mut)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestValidateCapabilityDomainManage(t *testing.T) {
	m := baseValid()
	m["capabilities"] = append(m["capabilities"].([]interface{}), map[string]interface{}{"kind": "domain:manage", "target": "mail"})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("domain:manage must be a known capability kind: %v", err)
	}
}
