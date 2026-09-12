package installer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// TestVerifySignatureGate exercises the three-way decision matrix Install
// uses before touching the DB: (1) keys configured → enforce, (2) no keys
// + AllowUnsigned → permit, (3) no keys + !AllowUnsigned → fail-closed.
func TestVerifySignatureGate(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	// Build a signed bundle the same way the hub publish flow would.
	src := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key:         "demo",
			Name:        "Demo",
			Description: "x",
			Version:     "1.0.0",
			Category:    "utility",
		},
	}
	var buf bytes.Buffer
	if err := bundle.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	digest := sha256.Sum256(buf.Bytes())
	sig := ed25519.Sign(priv, digest[:])

	parse := func() *bundle.Bundle {
		got, err := bundle.Read(bytes.NewReader(buf.Bytes()), 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return got
	}

	t.Run("fail closed when no keys and not allowed", func(t *testing.T) {
		i := &Installer{}
		if err := i.verifySignature(parse()); !errors.Is(err, ErrSignatureRequired) {
			t.Fatalf("want ErrSignatureRequired, got %v", err)
		}
	})

	t.Run("permit when AllowUnsigned and no keys", func(t *testing.T) {
		i := &Installer{AllowUnsigned: true}
		// Bundle has no manifest.Signature; permitted because dev mode is on.
		if err := i.verifySignature(parse()); err != nil {
			t.Fatalf("AllowUnsigned: %v", err)
		}
	})

	t.Run("reject unsigned when keys configured", func(t *testing.T) {
		i := &Installer{PublicKeys: []ed25519.PublicKey{pub}}
		// No Signature attached → rejected.
		if err := i.verifySignature(parse()); err == nil {
			t.Fatalf("want error, got nil")
		}
	})

	t.Run("accept signed bundle when keys match", func(t *testing.T) {
		i := &Installer{PublicKeys: []ed25519.PublicKey{pub}}
		b := parse()
		b.Manifest.Signature = &manifest.Signature{
			Algorithm: "ed25519",
			Digest:    hex.EncodeToString(digest[:]),
			Value:     hex.EncodeToString(sig),
		}
		if err := i.verifySignature(b); err != nil {
			t.Fatalf("verifySignature: %v", err)
		}
	})

	t.Run("reject signed bundle when keys mismatch", func(t *testing.T) {
		other, _, _ := ed25519.GenerateKey(rand.Reader)
		i := &Installer{PublicKeys: []ed25519.PublicKey{other}}
		b := parse()
		b.Manifest.Signature = &manifest.Signature{
			Algorithm: "ed25519",
			Digest:    hex.EncodeToString(digest[:]),
			Value:     hex.EncodeToString(sig),
		}
		if err := i.verifySignature(b); err == nil {
			t.Fatalf("want error, got nil")
		}
	})
}

// TestNewHonoursAllowUnsignedEvenWithReachableHub is the end-to-end
// regression for the live-verified gap: a kernel-native host constructed via
// New() with ALLOW_UNSIGNED_BUNDLES=true and a reachable MARKETPLACE_URL
// must still permit an unsigned bundle through Install's signature gate.
// Before the central_trust.go fix, New() fetched the central pubkey
// regardless of ALLOW_UNSIGNED_BUNDLES, populated PublicKeys, and made
// verifySignature's AllowUnsigned branch (guarded on len(PublicKeys)==0)
// unreachable — the exact failure observed against a real local `ops`
// instance (asteby-hq/addons PR #1409, packages/connector_whatsapp/
// AUDIT.md §12: "installer: bundle signature rejected: security: bundle
// has no signature" regardless of the env var).
func TestNewHonoursAllowUnsignedEvenWithReachableHub(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(marketplacePubKeyResponse{
			PubKey:    hex.EncodeToString(pub),
			Algorithm: "ed25519",
		})
	}))
	defer srv.Close()
	t.Setenv("MARKETPLACE_URL", srv.URL)
	t.Setenv("ALLOW_UNSIGNED_BUNDLES", "true")
	t.Setenv("MARKETPLACE_PUBKEY", "")
	t.Setenv("MARKETPLACE_PUBKEYS", "")

	inst := New(nil, "test")
	if len(inst.PublicKeys) != 0 {
		t.Fatalf("want PublicKeys empty so AllowUnsigned takes effect, got %d keys", len(inst.PublicKeys))
	}
	if !inst.AllowUnsigned {
		t.Fatalf("want AllowUnsigned true")
	}

	src := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key:         "demo",
			Name:        "Demo",
			Description: "x",
			Version:     "1.0.0",
			Category:    "utility",
		},
	}
	var buf bytes.Buffer
	if err := bundle.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := bundle.Read(bytes.NewReader(buf.Bytes()), 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := inst.verifySignature(b); err != nil {
		t.Fatalf("verifySignature: want unsigned dev bundle permitted, got %v", err)
	}
}

// TestVerifySignatureRejectsTamperedEntry exercises the per-file SHA-256
// branch added on top of the global Ed25519 check. The kernel's contract is:
// when the publisher stamps Signature.Checksums, mutating any single bundle
// entry — even one whose tarball SHA-256 still verifies against the signed
// digest because we patched EntryDigests separately — must produce
// ErrChecksumMismatch with the offending path in the message.
//
// We model "downstream tampering after Read" by mutating EntryDigests after
// bundle.Read has already populated it, since that's the same situation a
// host sees if a CDN edge or local cache substituted bytes between unpack
// and verifySignature.
func TestVerifySignatureRejectsTamperedEntry(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	src := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key:         "demo",
			Name:        "Demo",
			Description: "checksum-gate fixture",
			Version:     "1.0.0",
			Category:    "utility",
			Kernel:      ">=2.0.0 <3.0.0",
		},
		Migrations: []dynamic.File{{Version: "0001_init", SQL: "CREATE TABLE t ();\n"}},
		Frontend:   map[string][]byte{"frontend/remoteEntry.js": []byte("console.log('hello');")},
	}
	var buf bytes.Buffer
	if err := bundle.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	digest := sha256.Sum256(buf.Bytes())
	sig := ed25519.Sign(priv, digest[:])

	parsed, err := bundle.Read(bytes.NewReader(buf.Bytes()), 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	checksums := map[string]string{}
	for name, dig := range parsed.EntryDigests {
		if name == "manifest.json" {
			continue
		}
		checksums[name] = dig
	}
	parsed.Manifest.Signature = &manifest.Signature{
		Algorithm: "ed25519",
		Digest:    hex.EncodeToString(digest[:]),
		Value:     hex.EncodeToString(sig),
		Checksums: checksums,
	}

	i := &Installer{PublicKeys: []ed25519.PublicKey{pub}}

	t.Run("happy path with checksums", func(t *testing.T) {
		if err := i.verifySignature(parsed); err != nil {
			t.Fatalf("verifySignature: %v", err)
		}
	})

	t.Run("tampered entry rejected with named path", func(t *testing.T) {
		// Clone the parsed bundle so mutations do not leak into the happy
		// path subtest if the test order ever changes.
		tampered := *parsed
		tampered.EntryDigests = map[string]string{}
		for k, v := range parsed.EntryDigests {
			tampered.EntryDigests[k] = v
		}
		tampered.EntryDigests["frontend/remoteEntry.js"] = strings.Repeat("a", 64)
		err := i.verifySignature(&tampered)
		if err == nil {
			t.Fatalf("want error, got nil")
		}
		if !errors.Is(err, security.ErrChecksumMismatch) {
			t.Fatalf("want ErrChecksumMismatch, got %v", err)
		}
		if !strings.Contains(err.Error(), "frontend/remoteEntry.js") {
			t.Fatalf("error must name offending entry, got %v", err)
		}
	})
}
