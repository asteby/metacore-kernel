package installer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/manifest"
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
