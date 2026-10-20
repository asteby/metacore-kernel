package wasm

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// The "storefront_cart 0.2.2" state: the manifest gained exports (new routes)
// while the compiled module kept the old ones. Load swaps only the spec when
// the bytes are unchanged, so the mismatch must now be visible.
func TestMissingExports_ManifestAheadOfModule(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(ctx)

	old := &manifest.BackendSpec{Runtime: "wasm", Entry: "backend.wasm", Exports: []string{"echo"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", echoWasm(), old); err != nil {
		t.Fatal(err)
	}
	if m, loaded := h.MissingExports("testaddon"); !loaded || len(m) != 0 {
		t.Fatalf("coherent load reported drift: %v %v", m, loaded)
	}
	if len(h.ExportDrift()) != 0 {
		t.Fatalf("ExportDrift = %v", h.ExportDrift())
	}

	// Upgrade: same bytes, manifest now declares two more exports.
	next := &manifest.BackendSpec{Runtime: "wasm", Entry: "backend.wasm", Exports: []string{"echo", "address_post", "cart_get"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", echoWasm(), next); err != nil {
		t.Fatal(err)
	}
	m, loaded := h.MissingExports("testaddon")
	if !loaded || len(m) != 2 || m[0] != "address_post" || m[1] != "cart_get" {
		t.Fatalf("MissingExports = %v (loaded=%v)", m, loaded)
	}
	if d := h.ExportDrift(); len(d["testaddon"]) != 2 {
		t.Fatalf("ExportDrift = %v", d)
	}

	// Reload recompiles unconditionally and the host keeps working.
	if err := h.Reload(ctx, "testaddon", echoWasm(), next); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !h.IsCompiled("testaddon") {
		t.Fatal("not compiled after Reload")
	}
	if _, loaded := h.MissingExports("nope"); loaded {
		t.Fatal("unknown addon reported loaded")
	}
}
