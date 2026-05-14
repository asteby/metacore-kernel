//go:build (wasm || wasip1) && metacore_guest_alloc

package guest

import "unsafe"

// alloc is the bump allocator the host calls before writing a response
// envelope into guest memory. It is opt-in: addon authors who already
// export their own `alloc` (e.g. the minimal example in
// docs/wasm-abi.md § 4) keep using theirs. Build with
// `-tags metacore_guest_alloc` to pull in this default implementation.
//
// The function escapes `buf` to the heap by taking the address of its
// first element; that keeps the underlying array alive until the next
// instance is recycled. We allocate one buffer per call — sufficient
// for the small envelopes the host writes (a few KiB) and a fresh
// module instance per invocation means there's nothing to free.
//
//go:export alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		// Return a non-zero pointer so the host can distinguish "alloc
		// failed" (zero) from "zero-byte allocation succeeded". Any
		// stable address works; we point at a 1-byte sentinel that
		// also escapes to the heap to keep its address valid.
		sentinel := []byte{0}
		return uint32(uintptr(unsafe.Pointer(&sentinel[0])))
	}
	buf := make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}
