//go:build wasm || wasip1

package guest

import (
	"encoding/json"
	"unsafe"
)

// hostEventEmit is the raw host import.
//
//go:wasmimport metacore_host event_emit
func hostEventEmit(eventPtr, eventLen, payloadPtr, payloadLen uint32) uint64

// EmitEvent publishes `event` with `payload` through the kernel's
// in-process events.Bus. `payload` is marshalled to JSON via
// `encoding/json` and forwarded verbatim to subscribers.
//
// On success it returns the decoded EmitEventResult (with Subscribers,
// Meta, etc). On a `{success:false}` envelope it returns the zero result
// plus a typed `*EmitEventError`. A nil error never coexists with a
// failure envelope.
//
// Empty payloads are supported: passing `nil` (or a payload whose JSON
// encoding is `null`) skips the buffer altogether and the host delivers
// an explicit `nil` to subscriber handlers, per docs/wasm-abi.md § 12.3.
//
// Memory contract: the helper uses the guest's exported `alloc` only
// indirectly — the host writes the response envelope into guest memory
// via that same export before this function returns. The helper does
// NOT free the response buffer; the next module invocation gets a fresh
// instance so the bump arena is reclaimed automatically.
func EmitEvent(event string, payload any) (EmitEventResult, error) {
	// Marshal payload first so a JSON-encoding error surfaces before we
	// touch guest memory pointers. nil payloads short-circuit to
	// (0,0) — the host treats payloadLen==0 as "no payload" per
	// docs/wasm-abi.md § 12.3.
	var (
		payloadPtr uint32
		payloadLen uint32
	)
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return EmitEventResult{}, &EmitEventError{
				Code:    "invalid_payload",
				Message: "guest: marshal payload: " + err.Error(),
			}
		}
		// `null` is the JSON encoding of nil interface; treat it as
		// "no payload" rather than 4 wasted bytes on the wire.
		if !(len(buf) == 4 && buf[0] == 'n' && buf[1] == 'u' && buf[2] == 'l' && buf[3] == 'l') {
			payloadPtr, payloadLen = stringRef(string(buf))
		}
	}

	eventPtr, eventLen := stringRef(event)
	packed := hostEventEmit(eventPtr, eventLen, payloadPtr, payloadLen)

	envelope := readPacked(packed)
	return decodeEmitEnvelope(envelope)
}

// stringRef returns the (ptr, len) of `s`'s underlying byte buffer. The
// buffer is owned by the Go runtime and must not be mutated by the host
// — and we only pass it to the host for synchronous reads, so this is
// safe under both TinyGo (which uses a conservative GC) and the
// gc/wasm target.
//
// Using `unsafe.StringData` keeps us off the deprecated `reflect.StringHeader`
// path and works on TinyGo 0.30+ as well as Go 1.20+.
func stringRef(s string) (uint32, uint32) {
	if len(s) == 0 {
		return 0, 0
	}
	p := unsafe.StringData(s)
	return uint32(uintptr(unsafe.Pointer(p))), uint32(len(s))
}

// readPacked decodes the host's `(ptr<<32)|len` return value and reads
// the referenced bytes out of guest memory. A return of 0 means "no
// envelope" — equivalent to the pre-PR#62 success contract; the caller
// (decodeEmitEnvelope) treats that as a zero-value success.
//
// The returned slice references guest memory directly. Callers that
// need to retain it across host calls MUST copy first; today the only
// caller (decodeEmitEnvelope) consumes it synchronously via
// json.Unmarshal which copies into the decoded struct.
func readPacked(packed uint64) []byte {
	if packed == 0 {
		return nil
	}
	ptr := uint32(packed >> 32)
	n := uint32(packed & 0xFFFFFFFF)
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), n)
}
