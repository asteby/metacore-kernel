package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func concurrencyManifest(runtime, contributions string) string {
	rt := ""
	if runtime != "" {
		rt = `"runtime": ` + runtime + `,`
	}
	return `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "store_x", "name": "Store X", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  ` + rt + `
  "contributions": ` + contributions + `
}`
}

const (
	actionOnly = `{ "actions": [{ "key": "a", "label": "A", "target_model": "Sale", "handler": { "type": "wasm", "function": "do_a" } }] }`
	apiAndAct  = `{ "actions": [{ "key": "a", "label": "A", "target_model": "Sale", "handler": { "type": "wasm", "function": "do_a" } }],
	  "api": [{ "key": "cart_create", "method": "POST", "path": "/storefront/carts", "auth": "token", "scopes": ["cart:write"], "do": "wasm:cart_create" }] }`
)

func backendOf(t *testing.T, doc string) *manifest.BackendSpec {
	t.Helper()
	if err := v3.Validate([]byte(doc)); err != nil {
		t.Fatalf("v3.Validate: %v", err)
	}
	m, err := v3.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return manifest.FromV3(m).Backend
}

func TestFromV3_WasmConcurrency(t *testing.T) {
	// No API routes, no declaration: stays 0 (host default, serialised).
	if b := backendOf(t, concurrencyManifest("", actionOnly)); b == nil || b.Concurrency != 0 {
		t.Fatalf("plain addon: %+v", b)
	}
	// An addon that serves API routes gets the higher default.
	if b := backendOf(t, concurrencyManifest("", apiAndAct)); b == nil || b.Concurrency != manifest.DefaultAPIConcurrency {
		t.Fatalf("API addon default: %+v", b)
	}
	// An explicit value wins over the default, up or down.
	if b := backendOf(t, concurrencyManifest(`{ "wasm": { "concurrency": 8 } }`, apiAndAct)); b == nil || b.Concurrency != 8 {
		t.Fatalf("explicit: %+v", b)
	}
	if b := backendOf(t, concurrencyManifest(`{ "wasm": { "concurrency": 1 } }`, apiAndAct)); b == nil || b.Concurrency != 1 {
		t.Fatalf("explicit 1 on an API addon must stay 1: %+v", b)
	}
	if b := backendOf(t, concurrencyManifest(`{ "wasm": { "concurrency": 2 } }`, actionOnly)); b == nil || b.Concurrency != 2 {
		t.Fatalf("explicit on a plain addon: %+v", b)
	}
}

func TestV3Schema_RejectsOutOfRangeWasmConcurrency(t *testing.T) {
	for _, bad := range []string{`0`, `-1`, `65`, `"4"`} {
		doc := concurrencyManifest(`{ "wasm": { "concurrency": `+bad+` } }`, actionOnly)
		if err := v3.Validate([]byte(doc)); err == nil {
			t.Errorf("concurrency %s must be rejected", bad)
		}
	}
	if err := v3.Validate([]byte(concurrencyManifest(`{ "wasm": { "threads": 4 } }`, actionOnly))); err == nil {
		t.Error("unknown runtime.wasm field must be rejected")
	}
}

func TestValidate_BackendConcurrencyRange(t *testing.T) {
	mk := func(c int) *manifest.Manifest {
		return &manifest.Manifest{
			Key: "aa", Name: "A", Version: "1.0.0",
			Backend: &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Concurrency: c},
		}
	}
	for _, c := range []int{-1, manifest.MaxConcurrency + 1} {
		if err := mk(c).Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "concurrency") {
			t.Errorf("concurrency %d: want range error, got %v", c, err)
		}
	}
	if err := mk(8).Validate("2.0.0"); err != nil && strings.Contains(err.Error(), "concurrency") {
		t.Errorf("concurrency 8 must be accepted: %v", err)
	}
}
