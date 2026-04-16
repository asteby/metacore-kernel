package wasm

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// ABI convention (version 1):
//
//	The guest module MUST export:
//	  - memory              (default name "memory")
//	  - alloc(size i32) i32 — returns a ptr to `size` bytes of guest memory
//	  - <fn>(ptr i32, len i32) i64 — each export listed in BackendSpec.Exports
//
//	Return i64 packs result location: hi32 = ptr, lo32 = len. A zero return
//	means "empty success"; to signal errors, guest writes a JSON envelope
//	and the host surface layer decides how to interpret it.
//
//	The host module "metacore_host" provides:
//	  - log(msgPtr, msgLen)
//	  - env_get(keyPtr, keyLen) -> i64 (ptr|len, 0 if missing)
//	  - http_fetch(urlPtr, urlLen, methodPtr, methodLen, bodyPtr, bodyLen) -> i64

// writeMem allocates `len(data)` bytes in the guest via its exported alloc
// and copies data in. It returns the guest-side pointer.
func writeMem(ctx context.Context, mod api.Module, data []byte) (uint32, error) {
	alloc := mod.ExportedFunction("alloc")
	if alloc == nil {
		return 0, fmt.Errorf("wasm: guest missing `alloc` export")
	}
	res, err := alloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("wasm: alloc(%d): %w", len(data), err)
	}
	if len(res) == 0 {
		return 0, fmt.Errorf("wasm: alloc returned no value")
	}
	ptr := uint32(res[0])
	if len(data) == 0 {
		return ptr, nil
	}
	if !mod.Memory().Write(ptr, data) {
		return 0, fmt.Errorf("wasm: write %d bytes @ %d out of range", len(data), ptr)
	}
	return ptr, nil
}

// readMem reads a (ptr<<32)|len packed value out of guest memory. The ok
// flag is false when ptr+len would overflow the guest's current memory.
func readMem(mod api.Module, ptrLen uint64) ([]byte, bool) {
	if ptrLen == 0 {
		return nil, true
	}
	ptr := uint32(ptrLen >> 32)
	n := uint32(ptrLen & 0xFFFFFFFF)
	if n == 0 {
		return nil, true
	}
	b, ok := mod.Memory().Read(ptr, n)
	if !ok {
		return nil, false
	}
	return b, true
}

// packPtrLen is the host-side mirror of the guest's result encoding —
// used when a host-module import returns a buffer back to the guest.
func packPtrLen(ptr, length uint32) uint64 {
	return (uint64(ptr) << 32) | uint64(length)
}
