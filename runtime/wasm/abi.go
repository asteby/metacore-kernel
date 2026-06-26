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
//	  - http_request(urlPtr, urlLen, methodPtr, methodLen, headersPtr, headersLen, bodyPtr, bodyLen) -> i64
//	      Like http_fetch but with caller-supplied request headers (JSON object,
//	      e.g. {"Authorization":"token ..."}). Same http:fetch gate + SSRF guard
//	      + 30s timeout + 8 MiB cap. http_fetch delegates here with no headers.
//	  - connector_get(keyPtr, keyLen) -> i64
//	      Packed (ptr<<32)|len of the resolved connector credentials as a JSON
//	      object. Gated by connector:read <key>, tenant-scoped by the invocation
//	      orgID. See docs/wasm-abi.md § 1.6.
//	  - data_mutate(reqPtr, reqLen) -> i64
//	      Packed (ptr<<32)|len of the v1 `{success, data, meta}` envelope
//	      documented in docs/wasm-abi.md § 14. One org-scoped row mutation
//	      (create/update/delete) + a post-commit canonical event.
//	  - data_query(reqPtr, reqLen) -> i64
//	      Packed (ptr<<32)|len of the v1 `{success, data, meta}` envelope
//	      documented in docs/wasm-abi.md § 15. Read-only sibling of
//	      data_mutate: one org-scoped equality-filtered SELECT on a logical
//	      table (TableResolver, soft-delete aware). No events.
//	  - event_emit(eventPtr, eventLen, payloadPtr, payloadLen) -> i64
//	      Packed (ptr<<32)|len of the v1 `{success, data, meta}` envelope
//	      documented in docs/wasm-abi.md § 12.4. Legacy guests that ignore
//	      the return value still see the publish side-effect — the envelope
//	      is allocated in the guest's own bump arena but the host writes it
//	      *after* `events.Bus.PublishWithCount` returns, so dropping the
//	      return value is harmless. See wasm/eventemit.go (EventEmitEnvelopeVersion).

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
