package installer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
