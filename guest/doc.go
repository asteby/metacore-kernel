// Package guest provides TinyGo-compatible helpers that addon authors can
// import from inside a wasm backend to consume the metacore-kernel host
// imports without hand-rolling the `(ptr, len) -> i64` ABI on every call.
//
// The helpers mirror the host imports documented in
// docs/wasm-abi.md and translate the on-wire `{success, data, meta}`
// envelope into typed Go values plus a typed error.
//
// # Usage
//
//	import "github.com/asteby/metacore-kernel/guest"
//
//	//go:export resolve_ticket
//	func resolve_ticket(ptr, n uint32) uint64 {
//	    // ... handle request ...
//	    result, err := guest.EmitEvent("tickets.resolved",
//	        map[string]any{"id": id})
//	    if err != nil {
//	        // typed error: code-aware branching is possible via *guest.EmitEventError
//	        var emErr *guest.EmitEventError
//	        if errors.As(err, &emErr) && emErr.Code == "forbidden" { /* ... */ }
//	        return guest.WriteResult([]byte(`{"error":"emit_failed"}`))
//	    }
//	    _ = result.Subscribers
//	    return 0
//	}
//
// # alloc export
//
// The host writes envelope responses into guest memory via the guest's
// exported `alloc(size i32) i32`. This package exports a default `alloc`
// (a one-shot bump allocator backed by a Go `make([]byte, size)`) when
// built for a wasm target. Modules that already define their own `alloc`
// can stay on it — the symbols don't clash because we only emit ours
// under the `metacore_guest_alloc` build tag (see alloc_wasm.go).
//
// # TinyGo build
//
//	tinygo build -target=wasi -opt=z -no-debug \
//	    -o backend/backend.wasm ./backend/
//
// The helpers compile under TinyGo's `wasi` and `wasm-unknown` targets.
// The only stdlib used is `encoding/json`, `errors` and `unsafe` — all
// supported by TinyGo 0.30+.
package guest
