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
	"github.com/asteby/metacore-kernel/dynamic"
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

// buildBundleWithFrontend builds a small but realistic bundle (manifest +
// migration + frontend asset + readme) and returns it parsed-from-tar so
// EntryDigests is populated as it would be in production. The signature is
// over the unsigned tarball, exactly like buildSignedBundle, but the helper
// also returns the per-entry digests so callers can stamp Checksums into the
// out-of-band signature.
func buildBundleWithFrontend(t *testing.T, priv ed25519.PrivateKey) (*bundle.Bundle, [32]byte, []byte, map[string]string) {
	t.Helper()
	src := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key:         "demo",
			Name:        "Demo",
			Description: "checksums fixture",
			Version:     "1.0.0",
			Category:    "utility",
			Kernel:      ">=2.0.0 <3.0.0",
		},
		Migrations: []dynamic.File{{Version: "0001_init", SQL: "CREATE TABLE t ();\n"}},
		Frontend:   map[string][]byte{"frontend/remoteEntry.js": []byte("console.log('hello');")},
		Readme:     "# Demo\n",
	}
	var buf bytes.Buffer
	if err := bundle.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	digest := sha256.Sum256(buf.Bytes())
	sig := ed25519.Sign(priv, digest[:])

	got, err := bundle.Read(bytes.NewReader(buf.Bytes()), 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Stamp Checksums from the freshly-parsed entry digests, excluding
	// manifest.json (which cannot be self-described — see verifyEntryChecksums).
	checksums := map[string]string{}
	for name, dig := range got.EntryDigests {
		if name == "manifest.json" {
			continue
		}
		checksums[name] = dig
	}
	got.Manifest.Signature = &manifest.Signature{
		Algorithm: "ed25519",
		Digest:    hex.EncodeToString(digest[:]),
		Value:     hex.EncodeToString(sig),
		Checksums: checksums,
	}
	return got, digest, sig, checksums
}

func TestVerifyBundle_PerFileChecksumsValid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _, _ := buildBundleWithFrontend(t, priv)
	if err := security.VerifyBundle(b, []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
}

func TestVerifyBundle_PerFileChecksumMismatch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _, _ := buildBundleWithFrontend(t, priv)
	// Simulate a CDN serving a tampered remoteEntry.js: the EntryDigest the
	// caller sees diverges from what the publisher signed. Ed25519 over the
	// raw tarball still passes (we don't touch b.Raw — modeling a downstream
	// modification of the parsed bundle), so the per-file branch is what
	// catches it.
	b.EntryDigests["frontend/remoteEntry.js"] = strings.Repeat("a", 64)
	err := security.VerifyBundle(b, []ed25519.PublicKey{pub})
	if !errors.Is(err, security.ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "frontend/remoteEntry.js") {
		t.Fatalf("error must name the offending entry, got %v", err)
	}
}

func TestVerifyBundle_PerFileChecksumMissingEntry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _, _ := buildBundleWithFrontend(t, priv)
	// Publisher claims a checksum for a file the bundle never contained.
	// This is the "we promised to ship X but didn't" detection: an attacker
	// stripping a sensitive entry post-signing would land here.
	b.Manifest.Signature.Checksums["backend/backend.wasm"] = strings.Repeat("b", 64)
	err := security.VerifyBundle(b, []ed25519.PublicKey{pub})
	if !errors.Is(err, security.ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error must say missing, got %v", err)
	}
}

func TestVerifyBundle_PerFileChecksumExtraEntry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _, _ := buildBundleWithFrontend(t, priv)
	// An entry parsed out of the tarball but not declared in Checksums:
	// what an unsigned-payload-injection attack would look like if the
	// global digest were satisfied separately.
	b.EntryDigests["frontend/sneaky.js"] = strings.Repeat("c", 64)
	err := security.VerifyBundle(b, []ed25519.PublicKey{pub})
	if !errors.Is(err, security.ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("error must say not declared, got %v", err)
	}
}

func TestVerifyBundle_PerFileChecksumLegacyEmptyMapAccepted(t *testing.T) {
	// Bundles published before per-file checksums shipped leave Checksums nil
	// — the verifier must keep accepting them so we don't break installs in
	// the field on rollout. Equivalent to TestVerifyBundle_Valid but explicit
	// about the legacy contract.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b := buildSignedBundle(t, priv) // does not stamp Checksums
	if err := security.VerifyBundle(b, []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("legacy bundle without Checksums must verify: %v", err)
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
