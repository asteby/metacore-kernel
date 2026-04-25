package security_test

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
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// buildSignedBundle mirrors the hub publish flow:
//   - Publisher emits an UNSIGNED tarball.
//   - Hub computes digest = sha256(tarball) and signs the digest bytes.
//   - The signature is delivered out-of-band (catalog metadata, version row).
//   - At install time the host injects the catalog signature into the parsed
//     manifest BEFORE handing the Bundle to the installer.
//
// This helper produces a Bundle whose Raw matches an unsigned emit and whose
// Manifest.Signature is the out-of-band stamp — exactly what the verifier
// expects.
func buildSignedBundle(t *testing.T, priv ed25519.PrivateKey) *bundle.Bundle {
	t.Helper()
	src := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key:         "demo",
			Name:        "Demo",
			Description: "signed",
			Version:     "1.0.0",
			Category:    "utility",
			Kernel:      ">=2.0.0 <3.0.0",
		},
	}
	var buf bytes.Buffer
	if err := bundle.Write(&buf, src); err != nil {
		t.Fatalf("Write unsigned: %v", err)
	}
	digest := sha256.Sum256(buf.Bytes())
	sig := ed25519.Sign(priv, digest[:])

	got, err := bundle.Read(bytes.NewReader(buf.Bytes()), 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got.Manifest.Signature = &manifest.Signature{
		Algorithm: "ed25519",
		Digest:    hex.EncodeToString(digest[:]),
		Value:     hex.EncodeToString(sig),
	}
	return got
}

func TestVerifyBundle_Valid(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	b := buildSignedBundle(t, priv)
	if err := security.VerifyBundle(b, []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
}

func TestVerifyBundle_WrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	b := buildSignedBundle(t, priv)
	err := security.VerifyBundle(b, []ed25519.PublicKey{otherPub})
	if !errors.Is(err, security.ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch, got %v", err)
	}
}

func TestVerifyBundle_AnyOfMultipleKeys(t *testing.T) {
	// Key rotation scenario: two trusted keys, signature valid under one.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b := buildSignedBundle(t, priv)
	if err := security.VerifyBundle(b, []ed25519.PublicKey{otherPub, pub}); err != nil {
		t.Fatalf("VerifyBundle (rotation): %v", err)
	}
}

func TestVerifyBundle_TamperedBytes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b := buildSignedBundle(t, priv)
	// Flip a byte deep in the tarball — anywhere except the gzip header is
	// fine; the tar block at offset ~1024 is the manifest body.
	if len(b.Raw) < 2048 {
		t.Skip("bundle too small to mutate safely")
	}
	b.Raw[len(b.Raw)/2] ^= 0xFF
	err := security.VerifyBundle(b, []ed25519.PublicKey{pub})
	if err == nil {
		t.Fatalf("expected verification to fail on tampered bytes")
	}
}

func TestVerifyBundle_Unsigned(t *testing.T) {
	src := &bundle.Bundle{
		Manifest: manifest.Manifest{Key: "demo", Name: "Demo", Version: "1.0.0", Description: "x", Category: "utility"},
	}
	var buf bytes.Buffer
	if err := bundle.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := bundle.Read(&buf, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	err = security.VerifyBundle(got, []ed25519.PublicKey{pub})
	if !errors.Is(err, security.ErrUnsignedBundle) {
		t.Fatalf("want ErrUnsignedBundle, got %v", err)
	}
}

func TestVerifyBundle_UnsupportedAlgorithm(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b := buildSignedBundle(t, priv)
	b.Manifest.Signature.Algorithm = "rsa-pss"
	err := security.VerifyBundle(b, []ed25519.PublicKey{pub})
	if !errors.Is(err, security.ErrUnsupportedAlgorithm) {
		t.Fatalf("want ErrUnsupportedAlgorithm, got %v", err)
	}
}

func TestVerifyBundle_DigestDrift(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b := buildSignedBundle(t, priv)
	// Replace the manifest's declared digest with a clearly different one.
	b.Manifest.Signature.Digest = strings.Repeat("0", 64)
	err := security.VerifyBundle(b, []ed25519.PublicKey{pub})
	if err == nil || !strings.Contains(err.Error(), "digest drift") {
		t.Fatalf("want digest drift error, got %v", err)
	}
}

func TestParseHexPublicKeys(t *testing.T) {
	pubA, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB, _, _ := ed25519.GenerateKey(rand.Reader)
	csv := hex.EncodeToString(pubA) + " , " + hex.EncodeToString(pubB)
	keys, err := security.ParseHexPublicKeys(csv)
	if err != nil {
		t.Fatalf("ParseHexPublicKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}
	if !bytes.Equal(keys[0], pubA) || !bytes.Equal(keys[1], pubB) {
		t.Fatalf("keys not preserved")
	}
}

func TestParseHexPublicKeys_Empty(t *testing.T) {
	keys, err := security.ParseHexPublicKeys("")
	if err != nil || keys != nil {
		t.Fatalf("want nil/nil, got %v / %v", keys, err)
	}
}

func TestParseHexPublicKeys_Invalid(t *testing.T) {
	if _, err := security.ParseHexPublicKeys("not-hex"); err == nil {
		t.Fatalf("expected error on bad hex")
	}
	if _, err := security.ParseHexPublicKeys("aabbcc"); err == nil {
		t.Fatalf("expected error on wrong-length key")
	}
}
