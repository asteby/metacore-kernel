package wasm

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

func TestMemoryLimitPages(t *testing.T) {
	cases := []struct {
		mb   int
		want uint32
	}{
		{0, 4096}, {-1, 4096}, {1, 16}, {4, 64}, {256, 4096}, {1024, 4096},
	}
	for _, c := range cases {
		if got := memoryLimitPages(c.mb); got != c.want {
			t.Errorf("memoryLimitPages(%d) = %d, want %d", c.mb, got, c.want)
		}
	}
}

func newMemTestHost(t *testing.T) *Host {
	t.Helper()
	h, err := NewHost(context.Background(), security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

// echoWasmMinPages returns echoWasm with the memory minimum patched to pages
// (single-byte LEB128, so pages < 128).
func echoWasmMinPages(t *testing.T, pages byte) []byte {
	t.Helper()
	b := echoWasm()
	old := []byte{0x05, 0x03, 0x01, 0x00, 0x01}
	if !bytes.Contains(b, old) {
		t.Fatal("memory section not found in fixture")
	}
	return bytes.Replace(b, old, []byte{0x05, 0x03, 0x01, 0x00, pages}, 1)
}

func TestLoad_RejectsModuleOverMemoryLimit(t *testing.T) {
	ctx := context.Background()
	h := newMemTestHost(t)
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"echo"}, MemoryLimitMB: 1, TimeoutMs: 2000}
	// 32 pages = 2 MiB > 1 MiB limit.
	err := h.Load(ctx, "testaddon", echoWasmMinPages(t, 32), spec)
	if err == nil || !strings.Contains(err.Error(), "memory_limit_mb") {
		t.Fatalf("expected memory_limit_mb rejection, got %v", err)
	}
	if h.IsCompiled("testaddon") {
		t.Fatal("rejected module must not be cached")
	}
	// Same module with no limit (global ceiling) loads fine.
	spec.MemoryLimitMB = 0
	if err := h.Load(ctx, "testaddon", echoWasmMinPages(t, 32), spec); err != nil {
		t.Fatalf("Load with default limit: %v", err)
	}
}

func TestInvoke_EvictsInstanceThatGrewPastLimit(t *testing.T) {
	ctx := context.Background()
	h := newMemTestHost(t)
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"echo"}, MemoryLimitMB: 1, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	inst := uuid.New()
	if _, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("ok"), nil); err != nil {
		t.Fatalf("warm invoke: %v", err)
	}
	v, _ := h.modules.Load("testaddon|" + inst.String())
	mod := v.(*Module)
	if _, ok := mod.inst.Memory().Grow(32); !ok { // 33 pages > 16
		t.Fatal("grow failed")
	}
	_, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("big"), nil)
	if err == nil || !strings.Contains(err.Error(), "memory_limit_mb") {
		t.Fatalf("expected memory limit error, got %v", err)
	}
	if _, ok := h.modules.Load("testaddon|" + inst.String()); ok {
		t.Fatal("over-limit instance must be evicted")
	}
	// Next call gets a fresh instance within limits.
	out, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("again"), nil)
	if err != nil || string(out) != "again" {
		t.Fatalf("fresh instance invoke: %q %v", out, err)
	}
}
