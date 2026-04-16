package host

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/asteby/metacore-sdk/pkg/bundle"
	"github.com/asteby/metacore-sdk/pkg/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// TestHost_WASM_EndToEnd wires the minimal path a real host (ops/link) uses:
// EnableWASM → LoadWASMFromBundle(bundle with backend/backend.wasm) →
// InvokeWASM(export). Uses a hand-crafted module with `alloc` + `echo` so the
// test is pure stdlib + wazero (no tinygo required).
func TestHost_WASM_EndToEnd(t *testing.T) {
	ctx := context.Background()

	h := &Host{}
	caps := security.Compile("echo_addon", nil)
	if err := h.EnableWASM(ctx, caps); err != nil {
		t.Fatalf("EnableWASM: %v", err)
	}
	defer h.WASM.Close(ctx)

	// A minimal WASM module exporting memory, alloc(i32)->i32 and echo(ptr,len)->i64
	// that returns the same bytes back. The kernel/runtime/wasm package ships
	// the same encoder for its own tests — we reuse the test helper here by
	// constructing the module inline (keeps this test self-contained).
	wasmBytes := buildEchoModule()

	b := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key: "echo_addon",
			Backend: &manifest.BackendSpec{
				Runtime:  "wasm",
				Entry:    "backend/backend.wasm",
				Exports:  []string{"echo"},
			},
		},
		Backend: map[string][]byte{"backend/backend.wasm": wasmBytes},
	}
	if err := h.LoadWASMFromBundle(ctx, b); err != nil {
		t.Fatalf("LoadWASMFromBundle: %v", err)
	}

	payload := []byte(`{"ok":true}`)
	out, err := h.InvokeWASM(ctx, uuid.New(), "echo_addon", "echo", payload, nil)
	if err != nil {
		t.Fatalf("InvokeWASM: %v", err)
	}
	if string(out) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", out, payload)
	}
}

// buildEchoModule emits a minimal valid WASM module (hand-rolled) with:
//   memory export  (1 page)
//   func alloc(size i32) -> i32   { return 0 }         // bump at 0
//   func echo (ptr i32, len i32) -> i64 { return (uint64(ptr)<<32)|len }
//
// Guest memory at 0..N already holds the payload host wrote; echo returns the
// same (ptr,len) packed, which the host reads back.
func buildEchoModule() []byte {
	var buf []byte
	// magic + version
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	// type section: two function types
	//   0: (i32) -> i32        (alloc)
	//   1: (i32, i32) -> i64   (echo)
	typeSec := []byte{
		0x02,                   // 2 types
		0x60, 0x01, 0x7f, 0x01, 0x7f, // func (i32)->i32
		0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e, // func (i32,i32)->i64
	}
	buf = append(buf, section(1, typeSec)...)

	// function section: 2 funcs → types 0 and 1
	buf = append(buf, section(3, []byte{0x02, 0x00, 0x01})...)

	// memory section: 1 memory, min 1 page
	buf = append(buf, section(5, []byte{0x01, 0x00, 0x01})...)

	// export section
	exp := []byte{0x03} // 3 exports
	exp = appendExport(exp, "memory", 0x02, 0) // kind=mem, idx=0
	exp = appendExport(exp, "alloc", 0x00, 0)  // kind=func, idx=0
	exp = appendExport(exp, "echo", 0x00, 1)   // kind=func, idx=1
	buf = append(buf, section(7, exp)...)

	// code section
	//   alloc body: i32.const 0; end
	allocBody := []byte{0x00, 0x41, 0x00, 0x0b} // locals=0, i32.const 0, end
	//   echo body: local.get 0 (ptr); i64.extend_i32_u; i64.const 32; i64.shl
	//              local.get 1 (len); i64.extend_i32_u; i64.or; end
	echoBody := []byte{
		0x00,             // 0 local decls
		0x20, 0x00,       // local.get 0
		0xad,             // i64.extend_i32_u
		0x42, 0x20,       // i64.const 32
		0x86,             // i64.shl
		0x20, 0x01,       // local.get 1
		0xad,             // i64.extend_i32_u
		0x84,             // i64.or
		0x0b,             // end
	}
	code := []byte{0x02} // 2 entries
	code = append(code, uleb(uint32(len(allocBody)))...)
	code = append(code, allocBody...)
	code = append(code, uleb(uint32(len(echoBody)))...)
	code = append(code, echoBody...)
	buf = append(buf, section(10, code)...)

	return buf
}

func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(uint32(len(payload)))...)
	out = append(out, payload...)
	return out
}

func appendExport(dst []byte, name string, kind byte, idx uint32) []byte {
	dst = append(dst, uleb(uint32(len(name)))...)
	dst = append(dst, []byte(name)...)
	dst = append(dst, kind)
	dst = append(dst, uleb(idx)...)
	return dst
}

func uleb(v uint32) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
			continue
		}
		b = append(b, c)
		break
	}
	return b
}

var _ = binary.LittleEndian // keep the import around if refactored
