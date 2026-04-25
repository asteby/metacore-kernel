// Package security — bundle signature verification.
//
// The marketplace (hub) signs every published bundle with an offline Ed25519
// private key. The kernel verifies that signature with a hex-encoded public
// key supplied by the host (env var, config file, KMS — kernel does not care)
// before any addon code runs. This is the kernel's supply-chain guarantee:
// even if the bundle CDN, registry, or transport is compromised, an attacker
// cannot install code without also forging an Ed25519 signature.
//
// Wire format (must match hub/backend/internal/signer/signer.go):
//
//   - Digest = sha256(raw_bundle_bytes)              // hex-encoded into manifest.Signature.Digest
//   - Value  = ed25519.Sign(privKey, digest_bytes)   // hex-encoded into manifest.Signature.Value
//   - Algorithm = "ed25519"                          // case-insensitive
//
// The bundle that gets signed is the publisher's UNSIGNED tarball — the
// signature is shipped out-of-band (catalog version row, marketplace API)
// and the host injects it into Bundle.Manifest.Signature after parsing but
// before calling the installer. This avoids the chicken-and-egg of signing
// a payload that contains its own signature.
//
// The verifier therefore (a) recomputes sha256 from the captured raw tarball,
// (b) cross-checks it against Signature.Digest if present (to fail fast with a
// clearer error than ed25519.Verify would give), and (c) calls ed25519.Verify
// over the digest bytes with the supplied public key. Any mismatch is fatal.
//
// Multi-key trust: callers pass a slice so a publisher can rotate keys without
// downtime — the signature is accepted if it verifies under ANY of the trusted
// keys. Rotating in this model means: add the new key, re-sign new bundles
// with it, and remove the old key once no in-flight installs reference it.
package security

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/bundle"
)

// ErrUnsignedBundle is returned when a bundle has no Signature attached and
// the caller has not opted into the unsigned escape hatch. Hosts can use
// errors.Is to surface a 401/403 with a clear message.
var ErrUnsignedBundle = errors.New("security: bundle has no signature")

// ErrSignatureMismatch is returned when the signature does not verify under
// any of the supplied trusted keys.
var ErrSignatureMismatch = errors.New("security: signature does not verify under any trusted key")

// ErrUnsupportedAlgorithm is returned for unknown Algorithm fields. Today
// only "ed25519" is supported; future algorithms add new branches.
var ErrUnsupportedAlgorithm = errors.New("security: unsupported signature algorithm")

// VerifyBundle returns nil iff b carries a valid Ed25519 signature under any
// of trustedKeys. It does NOT consult environment variables or global state
// — pure function, easy to test.
//
// The bundle MUST have been produced by bundle.Read so that b.Raw is
// populated with the original compressed bytes; otherwise the digest cannot
// be recomputed and the function returns an error.
func VerifyBundle(b *bundle.Bundle, trustedKeys []ed25519.PublicKey) error {
	if b == nil {
		return errors.New("security: nil bundle")
	}
	if b.Manifest.Signature == nil || strings.TrimSpace(b.Manifest.Signature.Value) == "" {
		return ErrUnsignedBundle
	}
	if len(b.Raw) == 0 {
		return errors.New("security: bundle.Raw is empty (was the bundle constructed without bundle.Read?)")
	}
	if len(trustedKeys) == 0 {
		return errors.New("security: no trusted public keys configured")
	}

	sig := b.Manifest.Signature

	alg := strings.ToLower(strings.TrimSpace(sig.Algorithm))
	if alg == "" {
		alg = "ed25519"
	}
	if alg != "ed25519" {
		return fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, sig.Algorithm)
	}

	sigBytes, err := hex.DecodeString(strings.TrimSpace(sig.Value))
	if err != nil {
		return fmt.Errorf("security: signature is not hex: %w", err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("security: signature has %d bytes, want %d", len(sigBytes), ed25519.SignatureSize)
	}

	// Recompute the bundle digest from the original tarball bytes.
	sum := sha256.Sum256(b.Raw)
	digestHex := hex.EncodeToString(sum[:])

	// If the manifest declares a digest, fail fast on drift before the more
	// expensive ed25519 verify. This catches the "wrong tarball was uploaded
	// with the right signature" footgun with a clearer diagnostic.
	if want := strings.TrimSpace(sig.Digest); want != "" && want != digestHex {
		short := func(s string) string {
			if len(s) < 12 {
				return s
			}
			return s[:12]
		}
		return fmt.Errorf("security: digest drift (manifest=%s, computed=%s)",
			short(want), short(digestHex))
	}

	// Verify signature is over the raw 32-byte digest (matches hub Sign which
	// passes digest[:] to ed25519.Sign).
	for _, pub := range trustedKeys {
		if len(pub) != ed25519.PublicKeySize {
			continue // skip malformed keys; another may still match
		}
		if ed25519.Verify(pub, sum[:], sigBytes) {
			return nil
		}
	}
	return ErrSignatureMismatch
}

// ParseHexPublicKeys decodes a comma-separated list of hex-encoded Ed25519
// public keys. Whitespace around each entry is trimmed; empty entries are
// skipped. Used by hosts that load trusted keys from MARKETPLACE_PUBKEY or
// MARKETPLACE_PUBKEYS env vars. Returns an error on the first invalid entry
// so misconfiguration fails loudly at boot rather than silently at install.
func ParseHexPublicKeys(csv string) ([]ed25519.PublicKey, error) {
	if strings.TrimSpace(csv) == "" {
		return nil, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]ed25519.PublicKey, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw, err := hex.DecodeString(p)
		if err != nil {
			return nil, fmt.Errorf("security: invalid hex in public key list: %w", err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("security: public key has %d bytes, want %d", len(raw), ed25519.PublicKeySize)
		}
		out = append(out, ed25519.PublicKey(raw))
	}
	return out, nil
}
