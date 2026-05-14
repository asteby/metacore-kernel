package guest

import (
	"encoding/json"
	"fmt"
)

// HttpRequest is the input to HttpFetch. It mirrors the parameter
// surface of the host import documented in docs/wasm-abi.md § 3 and
// implemented in runtime/wasm/capabilities.go:142-182.
//
// The host import today accepts only `(url, method, body)` — there is
// no per-request header surface yet. `Headers` is reserved so addons
// that batch-author requests against the helper don't need to refactor
// when the host gains header support; today the field is silently
// ignored.
type HttpRequest struct {
	// Method is the HTTP verb. Empty defaults to GET on the host
	// (runtime/wasm/capabilities.go:155-157). Use the standard
	// `net/http` constants when running on stdlib Go; under TinyGo
	// hand-write the verb in uppercase.
	Method string
	// URL is the full absolute URL. The host enforces SSRF guards via
	// `Capabilities.CanFetch(url)` before any syscall — see
	// docs/permissions.md and the `http:fetch <host-whitelist>`
	// capability.
	URL string
	// Headers is reserved for a future host that supports
	// per-request headers. Today the field is silently ignored —
	// the host hard-codes Content-Type: application/json when the
	// body is non-empty. Populate it freely; helper preserves
	// forward-compat.
	Headers map[string][]string
	// Body is the raw request body. Nil / empty sends an empty body
	// and the host skips the Content-Type header.
	Body []byte
}

// HttpResponse is the decoded host response. The host envelope today
// is `{status, body}` (capabilities.go:175-180); response headers are
// not surfaced and the field is reserved for forward-compat.
type HttpResponse struct {
	// Status is the HTTP status code returned by the upstream server.
	// Zero when the host short-circuited before any request was made
	// (e.g. capability denied) — pair with the typed error to
	// distinguish.
	Status int
	// Headers is reserved for a future host that surfaces response
	// headers. Always nil today; helpers preserve the field so
	// consumers can be written against a stable shape.
	Headers map[string][]string
	// Body is the response body bytes. The host caps reads at 8 MiB
	// (capabilities.go:174). Larger responses are truncated silently.
	Body []byte
}

// HttpFetchError is the typed error HttpFetch returns when the host
// emits its `{error, message}` failure shape (capabilities.go:159-171
// uses the simpler `jsonError` shape rather than the
// `{success, data, meta}` envelope db_query / event_emit use).
//
// Defined codes:
//   - `forbidden`     — `Capabilities.CanFetch(url)` denied the call.
//                       Surfaced as the typed
//                       *HttpCapabilityDeniedError so callers can
//                       branch via `errors.As`.
//   - `bad_request`   — host couldn't build the request (invalid URL,
//                       malformed method).
//   - `transport`     — DNS / dial / TLS / read-body failure.
//   - `decode`        — host wrote something the helper couldn't
//                       parse. Indicates a host bug; surface verbatim.
type HttpFetchError struct {
	// Code is the machine-readable error code from the envelope.
	Code string
	// Message is the human-readable detail.
	Message string
}

// Error implements the error interface.
func (e *HttpFetchError) Error() string {
	if e == nil {
		return "guest: <nil http error>"
	}
	if e.Message == "" {
		return fmt.Sprintf("http_fetch: %s", e.Code)
	}
	return fmt.Sprintf("http_fetch: %s: %s", e.Code, e.Message)
}

// HttpCapabilityDeniedError is returned when the host denies the call
// via `Capabilities.CanFetch`. It embeds *HttpFetchError so callers
// can branch with `errors.As(err, &HttpCapabilityDeniedError{})` or
// with the broader `*HttpFetchError` via Unwrap.
type HttpCapabilityDeniedError struct {
	*HttpFetchError
}

// Unwrap exposes the embedded HttpFetchError so `errors.As(err,
// &HttpFetchError{})` matches a capability denial too. Without this
// the embedded pointer is not visible to the `errors` chain (the
// errors package walks Unwrap, not promoted fields).
func (e *HttpCapabilityDeniedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.HttpFetchError
}

// httpWireOK mirrors the success shape the host writes
// (capabilities.go:175-180): `{status, body}` — flat, no envelope.
type httpWireOK struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// httpWireErr mirrors the failure shape the host writes
// (capabilities.go:345 jsonError): `{error, message}` — also flat.
type httpWireErr struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// decodeHttpEnvelope parses the raw bytes the host wrote into guest
// memory into either a HttpResponse or a typed *HttpFetchError. The
// host's success and failure shapes share no fields — we probe for
// the failure shape first (presence of a non-empty `error` field)
// because the success shape always carries a numeric `status`.
//
// Exposed for tests that exercise the parser without going through
// the wasm import.
func decodeHttpEnvelope(buf []byte) (HttpResponse, error) {
	if len(buf) == 0 {
		// Host returned a zero packed value. Distinguish from a
		// successful empty-body response (which carries `status` even
		// when the body is empty) by surfacing a typed transport
		// error — the host always allocates a non-empty envelope
		// today, so an empty buffer is a wire-level anomaly.
		return HttpResponse{}, &HttpFetchError{
			Code:    "transport",
			Message: "host returned empty envelope",
		}
	}
	// Failure shape first — `error` is required on errors and absent
	// on successes, so this is unambiguous.
	var failure httpWireErr
	if err := json.Unmarshal(buf, &failure); err == nil && failure.Error != "" {
		he := &HttpFetchError{Code: failure.Error, Message: failure.Message}
		if he.Code == "forbidden" {
			return HttpResponse{}, &HttpCapabilityDeniedError{HttpFetchError: he}
		}
		return HttpResponse{}, he
	}
	var ok httpWireOK
	if err := json.Unmarshal(buf, &ok); err != nil {
		return HttpResponse{}, &HttpFetchError{
			Code:    "decode",
			Message: "guest: decode http_fetch envelope: " + err.Error(),
		}
	}
	return HttpResponse{
		Status: ok.Status,
		Body:   []byte(ok.Body),
	}, nil
}
