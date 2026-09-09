package guest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// TestDecodeHttpEnvelopeBinaryRoundTrip is the guest half of the #319 fix: the
// helper decodes the host's base64 transport transparently, so an addon that
// only ever reads HttpResponse.Body gets the upstream bytes verbatim — here a
// real PDF, the CFDI copy the SAT requires be kept for five years.
func TestDecodeHttpEnvelopeBinaryRoundTrip(t *testing.T) {
	pdf, err := os.ReadFile("testdata/binary_fixture.pdf")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	buf, err := json.Marshal(map[string]any{
		"status":         200,
		"body":           "",
		"body_base64":    base64.StdEncoding.EncodeToString(pdf),
		"body_is_base64": true,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	resp, err := decodeHttpEnvelope(buf)
	if err != nil {
		t.Fatalf("decodeHttpEnvelope: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("Status = %d, want 200", resp.Status)
	}
	if !resp.BodyIsBase64 {
		t.Fatal("BodyIsBase64 must report the transport the host used")
	}
	if !bytes.Equal(resp.Body, pdf) {
		t.Fatalf("Body did not round-trip: got %d bytes, want %d",
			len(resp.Body), len(pdf))
	}
}

// TestDecodeHttpEnvelopeTextPathUnchanged pins that a plain `{status, body}`
// envelope — everything the host wrote before this change, and everything it
// still writes for UTF-8 responses — decodes exactly as it always did.
func TestDecodeHttpEnvelopeTextPathUnchanged(t *testing.T) {
	buf := []byte(`{"status":201,"body":"{\"uuid\":\"7A1F\"}"}`)
	resp, err := decodeHttpEnvelope(buf)
	if err != nil {
		t.Fatalf("decodeHttpEnvelope: %v", err)
	}
	if resp.Status != 201 {
		t.Fatalf("Status = %d, want 201", resp.Status)
	}
	if resp.BodyIsBase64 {
		t.Fatal("BodyIsBase64 must be false on the plain path")
	}
	if got, want := string(resp.Body), `{"uuid":"7A1F"}`; got != want {
		t.Fatalf("Body = %q, want %q", got, want)
	}
}

// TestDecodeHttpEnvelopeBadBase64 asserts the helper refuses to hand back a
// partial body when the host flags base64 but writes something undecodable.
// Silence is precisely the failure mode #319 was about.
func TestDecodeHttpEnvelopeBadBase64(t *testing.T) {
	buf := []byte(`{"status":200,"body":"","body_base64":"!!not base64!!","body_is_base64":true}`)
	if _, err := decodeHttpEnvelope(buf); err == nil {
		t.Fatal("expected a decode error, got nil")
	}
}
