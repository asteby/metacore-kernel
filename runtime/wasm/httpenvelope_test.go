package wasm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"unicode/utf8"
)

// decodeEnvelope is the guest's half of the contract, kept deliberately
// verbatim-dumb: it is what an addon written against the ABI does with the
// bytes the host writes into its memory.
func decodeHTTPEnvelope(t *testing.T, buf []byte) (status int, body string, b64 string, isB64 bool) {
	t.Helper()
	var e struct {
		Status       int    `json:"status"`
		Body         string `json:"body"`
		BodyBase64   string `json:"body_base64"`
		BodyIsBase64 bool   `json:"body_is_base64"`
	}
	if err := json.Unmarshal(buf, &e); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	return e.Status, e.Body, e.BodyBase64, e.BodyIsBase64
}

// TestHTTPResponseEnvelopeTextUnchanged pins the pre-#319 behaviour for every
// UTF-8 response: `body` carries the string and the two base64 keys are absent
// from the wire entirely, so guests built against ABI 1.0-1.9 see no change.
func TestHTTPResponseEnvelopeTextUnchanged(t *testing.T) {
	for _, payload := range []string{
		`{"uuid":"7A1F-...","status":"stamped"}`,
		"plain ASCII",
		"acentos y ñ, 漢字, emoji 🧾",
	} {
		buf := httpResponseEnvelope(200, []byte(payload))
		if got := string(buf); bytes.Contains(buf, []byte("body_base64")) ||
			bytes.Contains(buf, []byte("body_is_base64")) {
			t.Fatalf("UTF-8 body must not carry the base64 keys, got %s", got)
		}
		status, body, _, isB64 := decodeHTTPEnvelope(t, buf)
		if status != 200 {
			t.Fatalf("status = %d, want 200", status)
		}
		if isB64 {
			t.Fatalf("body_is_base64 must be false for %q", payload)
		}
		if body != payload {
			t.Fatalf("body round-trip = %q, want %q", body, payload)
		}
	}
}

// TestHTTPResponseEnvelopeBinaryRoundTrip is the regression for #319: a REAL
// PDF — the SAT-mandated CFDI copy that exposed the bug — must reach the guest
// byte for byte. Before the fix encoding/json replaced every invalid UTF-8
// byte with U+FFFD and the guest persisted a corrupt file with a 200 status
// and no error anywhere.
func TestHTTPResponseEnvelopeBinaryRoundTrip(t *testing.T) {
	pdf, err := os.ReadFile("testdata/binary_fixture.pdf")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if utf8.Valid(pdf) {
		t.Fatal("fixture is valid UTF-8; it cannot exercise the binary path")
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatal("fixture is not a PDF")
	}

	buf := httpResponseEnvelope(200, pdf)
	status, body, b64, isB64 := decodeHTTPEnvelope(t, buf)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if !isB64 {
		t.Fatal("body_is_base64 must be true for a non-UTF-8 body")
	}
	if body != "" {
		t.Fatalf("body must be empty on the base64 path so a legacy guest "+
			"fails loudly instead of persisting garbage, got %d bytes", len(body))
	}
	got, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode body_base64: %v", err)
	}
	if !bytes.Equal(got, pdf) {
		t.Fatalf("PDF did not round-trip: got %d bytes, want %d", len(got), len(pdf))
	}

	// And the proof that the old transport was lossy, so this test fails if
	// anyone ever reverts to `"body": string(respBody)`.
	lossy, _ := json.Marshal(map[string]any{"body": string(pdf)})
	var back struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(lossy, &back); err != nil {
		t.Fatalf("unmarshal lossy: %v", err)
	}
	if back.Body == string(pdf) {
		t.Fatal("expected the plain-string transport to corrupt the PDF; " +
			"if it no longer does, this test's premise needs revisiting")
	}
}

// TestHTTPResponseEnvelopeEmptyBody covers the edge the ABI must not get wrong:
// an empty body is valid UTF-8, so it stays on the plain `body` path (a 204 or
// a HEAD must not suddenly look binary to a guest).
func TestHTTPResponseEnvelopeEmptyBody(t *testing.T) {
	for _, b := range [][]byte{nil, {}} {
		buf := httpResponseEnvelope(204, b)
		status, body, _, isB64 := decodeHTTPEnvelope(t, buf)
		if status != 204 {
			t.Fatalf("status = %d, want 204", status)
		}
		if isB64 {
			t.Fatal("an empty body must not take the base64 path")
		}
		if body != "" {
			t.Fatalf("body = %q, want empty", body)
		}
	}
}

// TestHTTPResponseEnvelopeSingleInvalidByte guards the detection boundary: one
// stray byte in an otherwise textual response is enough to switch transports,
// because that one byte is exactly what json.Marshal would silently rewrite.
func TestHTTPResponseEnvelopeSingleInvalidByte(t *testing.T) {
	raw := append([]byte("almost text"), 0xC3) // dangling UTF-8 lead byte
	buf := httpResponseEnvelope(200, raw)
	_, body, b64, isB64 := decodeHTTPEnvelope(t, buf)
	if !isB64 {
		t.Fatal("a single invalid byte must switch to the base64 transport")
	}
	if body != "" {
		t.Fatalf("body must be empty on the base64 path, got %q", body)
	}
	got, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode body_base64: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("round-trip = %v, want %v", got, raw)
	}
}
