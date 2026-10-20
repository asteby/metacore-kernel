package sigv2

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

// Conformance vector shared byte-for-byte with the hub
// (backend/internal/license/envelope_test.go). Ed25519 is deterministic, so the
// token for a fixed seed/kid/domain/payload is fixed: if either repo changes
// the wire format this test (in that repo) fails.
const (
	goldenSeedByte = 0x07
	goldenKID      = "legacy"
	goldenPayload  = `{"v":1,"typ":"instance","plan":"golden"}`
	goldenToken    = "asteby1.eyJraWQiOiJsZWdhY3kiLCJkb20iOiJhc3RlYnkvbGVhc2UvdjIiLCJhbGciOiJFZDI1NTE5In0.eyJ2IjoxLCJ0eXAiOiJpbnN0YW5jZSIsInBsYW4iOiJnb2xkZW4ifQ.3aUgHlw5jQRB6zYNe7quowIE_dQzYYd568ybbi1bYoxw4VKfmJS9xr0OFV71qqXZnbzAXbyAFVuAAuGhcsvhAA"
)

func TestGolden_EnvelopeVector(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{goldenSeedByte}, ed25519.SeedSize))
	got, err := Seal(priv, goldenKID, DomLease, []byte(goldenPayload))
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenToken {
		t.Fatalf("wire format changed:\n got  %s\n want %s", got, goldenToken)
	}
	ring := NewStaticKeyring().AddLegacy(priv.Public().(ed25519.PublicKey))
	o, err := Open(goldenToken, DomLease, ring, Policy{}, t0)
	if err != nil || string(o.Payload) != goldenPayload {
		t.Fatalf("golden does not open: %v", err)
	}
}
