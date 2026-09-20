package wasm

import (
	"context"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// trapWasm builds a module exporting alloc + `boom(ptr,len) -> i64`, where boom
// executes `unreachable` — the same trap a Go guest emits from fatalpanic.
func trapWasm() []byte {
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
	exports = append(exports, encodeName("boom")...)
	exports = append(exports, 0x00, 0x01)
	buf = append(buf, section(0x07, exports)...)
	allocBody := withSize([]byte{
		0x01, 0x01, 0x7F,
		0x23, 0x00, 0x22, 0x01, 0x20, 0x00, 0x6A, 0x24, 0x00, 0x20, 0x01,
		0x0B,
	})
	boomBody := withSize([]byte{0x00, 0x00, 0x0B}) // no locals; unreachable; end
	code := []byte{0x02}
	code = append(code, allocBody...)
	code = append(code, boomBody...)
	return append(buf, section(0x0A, code)...)
}

// A guest trap leaves the Go runtime inside the module half-unwound: the next
// call on the same instance dies with "unreachable" / "invalid table access"
// (QA 2026-09-20 F-VEN-08, H-14). The host must evict the instance on any trap
// so the following invocation runs on a fresh reactor.
func TestHost_EvictsInstanceAfterGuestTrap(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer h.Close(ctx)

	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "backend.wasm", Exports: []string{"boom"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", trapWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	installation := uuid.New()
	_, err = h.Invoke(ctx, installation, "testaddon", "boom", []byte("x"), nil)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("want unreachable trap, got %v", err)
	}
	if _, ok := h.modules.Load("testaddon|" + installation.String()); ok {
		t.Fatal("poisoned instance still cached after a guest trap")
	}
}
