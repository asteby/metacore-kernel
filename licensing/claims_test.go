package licensing

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mintToken signs a Claims payload with priv exactly as the hub does: compact
// json.Marshal, ed25519.Sign over the raw bytes, base64url(payload).sig.
func mintToken(t *testing.T, priv ed25519.PrivateKey, c Claims) string {
	t.Helper()
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Type == "" {
		c.Type = claimType
	}
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	sig := ed25519.Sign(priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

func testKeypair(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return priv, hex.EncodeToString(pub)
}

func baseClaims() Claims {
	now := time.Now().UTC()
	return Claims{
		Version:    1,
		Type:       claimType,
		LicenseID:  uuid.New(),
		CustomerID: uuid.New(),
		Presets:    []string{"pitsline"},
		Addons:     []string{"workshop", "inventory"},
		IssuedAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(365 * 24 * time.Hour),
	}
}

func TestVerify_ValidToken(t *testing.T) {
	priv, pubHex := testKeypair(t)
	tok := mintToken(t, priv, baseClaims())

	c, err := Verify(tok, pubHex, time.Now().UTC())
	if err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if len(c.Presets) != 1 || c.Presets[0] != "pitsline" {
		t.Errorf("presets = %v", c.Presets)
	}
	// Both the addon keys and the preset key itself are entitled locally.
	if !c.Entitles("workshop") || !c.Entitles("pitsline") || c.Entitles("nope") {
		t.Errorf("entitlement resolution wrong")
	}
}

func TestVerify_Wildcard(t *testing.T) {
	priv, pubHex := testKeypair(t)
	cl := baseClaims()
	cl.Addons = []string{"*"}
	tok := mintToken(t, priv, cl)

	c, err := Verify(tok, pubHex, time.Now().UTC())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !c.Wildcard() || !c.Entitles("anything-at-all") {
		t.Errorf("wildcard should entitle everything")
	}
}

func TestVerify_Expired(t *testing.T) {
	priv, pubHex := testKeypair(t)
	cl := baseClaims()
	cl.IssuedAt = time.Now().Add(-48 * time.Hour)
	cl.ExpiresAt = time.Now().Add(-24 * time.Hour)
	tok := mintToken(t, priv, cl)

	c, err := Verify(tok, pubHex, time.Now().UTC())
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
	// Claims still returned so the caller can apply grace.
	if c == nil || !c.Entitles("workshop") {
		t.Errorf("expired token should still expose claims for grace")
	}
}

func TestVerify_NotYetValid(t *testing.T) {
	priv, pubHex := testKeypair(t)
	cl := baseClaims()
	cl.IssuedAt = time.Now().Add(48 * time.Hour)
	cl.ExpiresAt = time.Now().Add(96 * time.Hour)
	tok := mintToken(t, priv, cl)

	_, err := Verify(tok, pubHex, time.Now().UTC())
	if !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("expected ErrNotYetValid, got %v", err)
	}
}

func TestVerify_TamperedSignature(t *testing.T) {
	priv, pubHex := testKeypair(t)
	tok := mintToken(t, priv, baseClaims())
	// Flip a character in the MIDDLE of the signature segment. Flipping the
	// last one is flaky: base64url ignores the unused padding bits of the
	// final symbol, so A<->B there may decode to the same signature bytes.
	dot := strings.IndexByte(tok, '.')
	mid := dot + 1 + (len(tok)-dot-1)/2
	flip := byte('A')
	if tok[mid] == 'A' {
		flip = 'B'
	}
	tampered := tok[:mid] + string(flip) + tok[mid+1:]
	if _, err := Verify(tampered, pubHex, time.Now().UTC()); !errors.Is(err, ErrBadSig) && !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected bad-sig/malformed, got %v", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	priv, _ := testKeypair(t)
	_, otherPubHex := testKeypair(t)
	tok := mintToken(t, priv, baseClaims())
	if _, err := Verify(tok, otherPubHex, time.Now().UTC()); !errors.Is(err, ErrBadSig) {
		t.Fatalf("expected ErrBadSig against wrong key, got %v", err)
	}
}

func TestVerify_NoPubKey(t *testing.T) {
	priv, _ := testKeypair(t)
	tok := mintToken(t, priv, baseClaims())
	if _, err := Verify(tok, "", time.Now().UTC()); !errors.Is(err, ErrNoPubKey) {
		t.Fatalf("expected ErrNoPubKey, got %v", err)
	}
}

func TestVerify_UnsupportedVersion(t *testing.T) {
	priv, pubHex := testKeypair(t)
	cl := baseClaims()
	cl.Version = 2
	tok := mintToken(t, priv, cl)
	if _, err := Verify(tok, pubHex, time.Now().UTC()); !errors.Is(err, ErrVersion) {
		t.Fatalf("expected ErrVersion, got %v", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	_, pubHex := testKeypair(t)
	for _, bad := range []string{"", "onlyonepart", "a.b.c", "!!!.@@@"} {
		if _, err := Verify(bad, pubHex, time.Now().UTC()); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestVerify_WrongTypeRejected(t *testing.T) {
	priv, pubHex := testKeypair(t)
	cl := baseClaims()
	cl.Type = "trial"
	tok := mintToken(t, priv, cl)
	if _, err := Verify(tok, pubHex, time.Now().UTC()); !errors.Is(err, ErrWrongType) {
		t.Fatalf("expected ErrWrongType, got %v", err)
	}
}

func TestClaims_LeaseExpired(t *testing.T) {
	now := time.Now().UTC()
	cl := baseClaims()
	cl.MaxOfflineHours = 72
	cl.IssuedAt = now.Add(-73 * time.Hour)
	if !cl.LeaseExpired(now) {
		t.Fatalf("73h offline with a 72h lease must be stale")
	}
	cl.IssuedAt = now.Add(-1 * time.Hour) // a renew re-stamps iat
	if cl.LeaseExpired(now) {
		t.Fatalf("fresh check-in must clear the lease")
	}
	cl.MaxOfflineHours = 0
	cl.IssuedAt = now.Add(-1000 * time.Hour)
	if cl.LeaseExpired(now) {
		t.Fatalf("lease disabled (0) must never go stale")
	}
}

func TestVerify_UnlimitedNeverExpiresNorLeases(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	cl := baseClaims()
	cl.Plan = "unlimited"
	cl.Addons = []string{"*"}
	cl.MaxOfflineHours = 24
	cl.IssuedAt = now.Add(-100 * 24 * time.Hour)
	cl.ExpiresAt = now.Add(-24 * time.Hour) // past exp: unlimited still verifies
	tok := mintToken(t, priv, cl)
	c, err := Verify(tok, pubHex, now)
	if err != nil {
		t.Fatalf("unlimited must verify past exp, got %v", err)
	}
	if !c.Unlimited() || c.LeaseExpired(now) {
		t.Fatalf("unlimited must be lease-exempt: %+v", c)
	}
}
