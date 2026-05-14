package guest

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeHttpEnvelope_SuccessHappyPath(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"status": 200,
		"body":   `{"ok":true}`,
	})
	got, err := decodeHttpEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeHttpEnvelope: %v", err)
	}
	if got.Status != 200 {
		t.Errorf("Status = %d, want 200", got.Status)
	}
	if string(got.Body) != `{"ok":true}` {
		t.Errorf("Body = %q, want {\"ok\":true}", string(got.Body))
	}
}

func TestDecodeHttpEnvelope_SuccessEmptyBody(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"status": 204,
		"body":   "",
	})
	got, err := decodeHttpEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeHttpEnvelope: %v", err)
	}
	if got.Status != 204 {
		t.Errorf("Status = %d, want 204", got.Status)
	}
	if len(got.Body) != 0 {
		t.Errorf("Body = %q, want empty", string(got.Body))
	}
}

func TestDecodeHttpEnvelope_ForbiddenReturnsTypedDenied(t *testing.T) {
	// Host emits the `jsonError` shape: {error, message}. The
	// "forbidden" code triggers the dedicated capability-denied error
	// type so callers can branch via errors.As.
	raw := mustMarshalAny(t, map[string]any{
		"error":   "forbidden",
		"message": "addon \"tickets\" lacks http:fetch \"api.stripe.com\"",
	})
	_, err := decodeHttpEnvelope(raw)
	if err == nil {
		t.Fatal("expected forbidden error")
	}
	var denied *HttpCapabilityDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error is not *HttpCapabilityDeniedError: %T (%v)", err, err)
	}
	if denied.Code != "forbidden" {
		t.Errorf("Code = %q, want forbidden", denied.Code)
	}
	// Also matches the broader *HttpFetchError type.
	var generic *HttpFetchError
	if !errors.As(err, &generic) {
		t.Errorf("forbidden error not assignable to *HttpFetchError")
	}
}

func TestDecodeHttpEnvelope_TransportReturnsTypedError(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"error":   "transport",
		"message": "dial tcp: lookup foo.invalid: no such host",
	})
	_, err := decodeHttpEnvelope(raw)
	if err == nil {
		t.Fatal("expected transport error")
	}
	var fe *HttpFetchError
	if !errors.As(err, &fe) {
		t.Fatalf("not *HttpFetchError: %T", err)
	}
	if fe.Code != "transport" {
		t.Errorf("Code = %q, want transport", fe.Code)
	}
	// transport is NOT a capability denial — must not match
	// HttpCapabilityDeniedError.
	var denied *HttpCapabilityDeniedError
	if errors.As(err, &denied) {
		t.Errorf("transport error wrongly classified as capability denial")
	}
}

func TestDecodeHttpEnvelope_BadRequestReturnsTypedError(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"error":   "bad_request",
		"message": "parse \"::bad::\": invalid url",
	})
	_, err := decodeHttpEnvelope(raw)
	var fe *HttpFetchError
	if !errors.As(err, &fe) {
		t.Fatalf("not *HttpFetchError: %T", err)
	}
	if fe.Code != "bad_request" {
		t.Errorf("Code = %q, want bad_request", fe.Code)
	}
}

func TestDecodeHttpEnvelope_EmptyBufferIsTransportError(t *testing.T) {
	// Host always allocates a non-empty envelope today. An empty
	// buffer is a wire-level anomaly — surface as transport so the
	// addon's existing retry path catches it.
	_, err := decodeHttpEnvelope(nil)
	if err == nil {
		t.Fatal("expected error on empty buffer")
	}
	var fe *HttpFetchError
	if !errors.As(err, &fe) {
		t.Fatalf("not *HttpFetchError: %T", err)
	}
	if fe.Code != "transport" {
		t.Errorf("Code = %q, want transport", fe.Code)
	}
}

func TestDecodeHttpEnvelope_MalformedJSON(t *testing.T) {
	_, err := decodeHttpEnvelope([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected decode error")
	}
	var fe *HttpFetchError
	if !errors.As(err, &fe) {
		t.Fatalf("not *HttpFetchError: %T", err)
	}
	if fe.Code != "decode" {
		t.Errorf("Code = %q, want decode", fe.Code)
	}
}

func TestHttpFetchError_Error(t *testing.T) {
	var nilErr *HttpFetchError
	if got := nilErr.Error(); got == "" {
		t.Errorf("nil receiver returned empty string")
	}
	e := &HttpFetchError{Code: "forbidden", Message: "no cap"}
	if got := e.Error(); got != "http_fetch: forbidden: no cap" {
		t.Errorf("unexpected: %q", got)
	}
	e2 := &HttpFetchError{Code: "transport"}
	if got := e2.Error(); got != "http_fetch: transport" {
		t.Errorf("unexpected: %q", got)
	}
}

// mustMarshalAny is a local twin of mustMarshal (defined in
// eventemit_test.go) so the test file is self-contained and easy to
// audit. Both helpers do the same thing — fixture marshalling — and
// neither is exported.
func mustMarshalAny(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}
