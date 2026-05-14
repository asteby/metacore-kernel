//go:build wasm || wasip1

package guest

import "unsafe"

// hostHttpFetch is the raw host import. The signature mirrors
// runtime/wasm/capabilities.go:142-182.
//
//go:wasmimport metacore_host http_fetch
func hostHttpFetch(urlPtr, urlLen, methodPtr, methodLen, bodyPtr, bodyLen uint32) uint64

// HttpFetch performs an outbound HTTP request through the host. The
// call is subject to the addon's `http:fetch <host-whitelist>`
// capability and the egress SSRF guard (see docs/permissions.md).
//
// On `forbidden` the helper returns a typed
// `*HttpCapabilityDeniedError` so callers can branch:
//
//	resp, err := guest.HttpFetch(guest.HttpRequest{URL: "..."})
//	var denied *guest.HttpCapabilityDeniedError
//	if errors.As(err, &denied) { /* missing capability */ }
//
// Memory contract: the body buffer is owned by the caller and read
// synchronously by the host before this function returns; it is safe
// to reuse `req.Body` after the call.
func HttpFetch(req HttpRequest) (HttpResponse, error) {
	urlPtr, urlLen := stringRef(req.URL)
	methodPtr, methodLen := stringRef(req.Method)
	bodyPtr, bodyLen := bytesRef(req.Body)
	packed := hostHttpFetch(urlPtr, urlLen, methodPtr, methodLen, bodyPtr, bodyLen)
	envelope := readPacked(packed)
	return decodeHttpEnvelope(envelope)
}

// bytesRef returns the (ptr, len) of `b`'s underlying buffer. Mirrors
// stringRef for the request-body case where the input is already a
// byte slice. Empty / nil slices return (0, 0) which the host treats
// as "no body" — see capabilities.go:165-167 (skips Content-Type when
// body is empty).
func bytesRef(b []byte) (uint32, uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}
