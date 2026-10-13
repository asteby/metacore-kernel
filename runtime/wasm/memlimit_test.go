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

// growWasm builds a module exporting alloc + `grow(ptr,len) -> i64` that calls
// memory.grow(len) (so the payload LENGTH is the number of pages to add) and
// executes `unreachable` when the engine refuses (-1) — what a Go guest does
// on out-of-memory. On success it returns 0 (empty result).
func growWasm() []byte {
	buf := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, section(0x01, []byte{
		0x02,
		0x60, 0x01, 0x7F, 0x01, 0x7F,
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E,
	})...)
	buf = append(buf, section(0x03, []byte{0x02, 0x00, 0x01})...)
	buf = append(buf, section(0x05, []byte{0x01, 0x00, 0x01})...)
	globals := []byte{0x01, 0x7F, 0x01, 0x41}
	globals = append(globals, encodeSLEB128(1024)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)
	var exports []byte
	exports = append(exports, 0x03)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x00)
	exports = append(exports, encodeName("grow")...)
	exports = append(exports, 0x00, 0x01)
	buf = append(buf, section(0x07, exports)...)
	allocBody := withSize([]byte{
		0x01, 0x01, 0x7F,
		0x23, 0x00, 0x22, 0x01, 0x20, 0x00, 0x6A, 0x24, 0x00, 0x20, 0x01,
		0x0B,
	})
	growBody := withSize([]byte{
		0x00,       // no locals
		0x20, 0x01, // local.get 1 (pages)
		0x40, 0x00, // memory.grow 0
		0x41, 0x7F, // i32.const -1
		0x46,       // i32.eq
		0x04, 0x40, // if (void)
		0x00,       //   unreachable
		0x0B,       // end
		0x42, 0x00, // i64.const 0
		0x0B,
	})
	code := []byte{0x02}
	code = append(code, allocBody...)
	code = append(code, growBody...)
	return append(buf, section(0x0A, code)...)
}

// The per-addon memory limit is enforced INSIDE the call by the runtime built
// for that limit: memory.grow past it fails mid-call (the guest traps), the
// post-call size check is only a backstop.
func TestInvoke_HardMemoryLimitInsideCall(t *testing.T) {
	ctx := context.Background()
	h := newMemTestHost(t)
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"grow"}, MemoryLimitMB: 1, TimeoutMs: 2000} // 16 pages
	if err := h.Load(ctx, "testaddon", growWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	inst := uuid.New()
	// 1 -> 11 pages: within the limit.
	if _, err := h.Invoke(ctx, inst, "testaddon", "grow", make([]byte, 10), nil); err != nil {
		t.Fatalf("grow within limit: %v", err)
	}
	// +10 more (21 > 16): refused by the engine mid-call, NOT by the
	// post-call check (which would have said "exceeded memory_limit_mb").
	_, err := h.Invoke(ctx, inst, "testaddon", "grow", make([]byte, 10), nil)
	if err == nil || !strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "exceeded memory_limit_mb") {
		t.Fatalf("want in-call trap from refused memory.grow, got %v", err)
	}
	if idle, busy := h.poolStats("testaddon", inst); idle != 0 || busy != 0 {
		t.Fatalf("trapped instance must be evicted (idle=%d busy=%d)", idle, busy)
	}
	// Fresh instance works again.
	if _, err := h.Invoke(ctx, inst, "testaddon", "grow", make([]byte, 10), nil); err != nil {
		t.Fatalf("fresh instance: %v", err)
	}
}

// The same module under a generous limit grows fine: limits are per addon, not
// global, and both runtimes share one compilation cache.
func TestInvoke_MemoryLimitIsPerAddon(t *testing.T) {
	ctx := context.Background()
	h := newMemTestHost(t)
	tight := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"grow"}, MemoryLimitMB: 1, TimeoutMs: 2000}
	loose := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"grow"}, MemoryLimitMB: 8, TimeoutMs: 2000}
	if err := h.Load(ctx, "tight", growWasm(), tight); err != nil {
		t.Fatal(err)
	}
	if err := h.Load(ctx, "loose", growWasm(), loose); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 40) // 41 pages: > 16, < 128
	if _, err := h.Invoke(ctx, uuid.New(), "tight", "grow", payload, nil); err == nil {
		t.Fatal("tight addon must not grow past its limit")
	}
	if _, err := h.Invoke(ctx, uuid.New(), "loose", "grow", payload, nil); err != nil {
		t.Fatalf("loose addon should grow: %v", err)
	}
	n := 0
	h.runtimes.Range(func(_, _ any) bool { n++; return true })
	if n != 3 { // global + 16 pages + 128 pages
		t.Fatalf("want 3 runtimes (global,1MB,8MB), got %d", n)
	}
}
