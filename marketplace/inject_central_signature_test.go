package marketplace

import (
	"net/http"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/manifest"
)

// Central trust anchor delivers the bundle signature out-of-band via the
// X-Asteby-Marketplace-Signature response header. injectCentralSignature
// hoists it onto manifest.Signature so the security gate has something to
// verify; the precedence and edge cases below match what the install +
// upgrade HTTP paths rely on.

func TestInjectCentralSignature_FromHeader(t *testing.T) {
	b := &bundle.Bundle{}
	h := http.Header{}
	h.Set(installer.HeaderMarketplaceSignature, "deadbeef")
	h.Set(installer.HeaderBundleChecksum, "cafe")

	injectCentralSignature(b, h)

	if b.Manifest.Signature == nil {
		t.Fatal("expected Signature to be set from header")
	}
	if b.Manifest.Signature.Value != "deadbeef" {
		t.Errorf("Value = %q, want deadbeef", b.Manifest.Signature.Value)
	}
	if b.Manifest.Signature.Digest != "cafe" {
		t.Errorf("Digest = %q, want cafe", b.Manifest.Signature.Digest)
	}
	if b.Manifest.Signature.Algorithm != "ed25519" {
		t.Errorf("Algorithm = %q, want ed25519", b.Manifest.Signature.Algorithm)
	}
	if !b.Manifest.Signature.Verified {
		t.Error("Verified should be true after central counter-sign injection")
	}
}

func TestInjectCentralSignature_OverwritesPublisherSignature(t *testing.T) {
	// A publisher-embedded signature (e.g. CI signs with
	// ADDON_SIGNING_KEY_ED25519 before uploading to the hub) is provenance,
	// not a trust path: a host that only configures MARKETPLACE_PUBKEY (the
	// common/production case) cannot verify it. The central header signature
	// must always win, or installs of pre-signed bundles fail closed against
	// a key the host never trusted.
	b := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Signature: &manifest.Signature{
				Algorithm: "ed25519",
				Value:     "publisher-sig",
			},
		},
	}
	h := http.Header{}
	h.Set(installer.HeaderMarketplaceSignature, "central-sig")

	injectCentralSignature(b, h)

	if b.Manifest.Signature.Value != "central-sig" {
		t.Errorf("central signature should overwrite publisher signature: got %q", b.Manifest.Signature.Value)
	}
}

func TestInjectCentralSignature_KeepsPublisherSignatureWhenNoCentralHeader(t *testing.T) {
	// Local/dev hubs without MARKETPLACE_SIGNING_SEED configured ship no
	// central header at all — fall back to whatever signature (if any) the
	// bundle already carried rather than clearing it.
	b := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Signature: &manifest.Signature{
				Algorithm: "ed25519",
				Value:     "publisher-sig",
			},
		},
	}
	injectCentralSignature(b, http.Header{})

	if b.Manifest.Signature.Value != "publisher-sig" {
		t.Errorf("publisher signature should be kept when no central header present: got %q", b.Manifest.Signature.Value)
	}
}

func TestInjectCentralSignature_EmptyHeaderNoChange(t *testing.T) {
	b := &bundle.Bundle{}
	injectCentralSignature(b, http.Header{})
	if b.Manifest.Signature != nil {
		t.Errorf("Signature should remain nil when header missing; got %+v", b.Manifest.Signature)
	}
}

func TestInjectCentralSignature_HeaderWhitespaceTrimmed(t *testing.T) {
	b := &bundle.Bundle{}
	h := http.Header{}
	h.Set(installer.HeaderMarketplaceSignature, "  abcd  ")
	h.Set(installer.HeaderBundleChecksum, "  ef01  ")
	injectCentralSignature(b, h)
	if b.Manifest.Signature == nil {
		t.Fatal("expected Signature to be set")
	}
	if b.Manifest.Signature.Value != "abcd" {
		t.Errorf("Value = %q, want trimmed 'abcd'", b.Manifest.Signature.Value)
	}
	if b.Manifest.Signature.Digest != "ef01" {
		t.Errorf("Digest = %q, want trimmed 'ef01'", b.Manifest.Signature.Digest)
	}
}

func TestInjectCentralSignature_HeaderConstantsMatchHub(t *testing.T) {
	// Sanity check: the header constants must literally match what hub serves.
	// If someone renames either side this test trips before a real install
	// fails in prod with "bundle has no signature".
	if installer.HeaderMarketplaceSignature != "X-Asteby-Marketplace-Signature" {
		t.Errorf("HeaderMarketplaceSignature drifted: %q", installer.HeaderMarketplaceSignature)
	}
	if installer.HeaderBundleChecksum != "X-Bundle-Checksum" {
		t.Errorf("HeaderBundleChecksum drifted: %q", installer.HeaderBundleChecksum)
	}
	// Case-insensitive header lookup contract — http.Header.Get is canonicalised
	// but downstream tooling may compare strings; lock the spelling.
	if got := strings.ToLower(installer.HeaderMarketplaceSignature); got != "x-asteby-marketplace-signature" {
		t.Errorf("lowercase form drifted: %q", got)
	}
}

func TestInjectCentralSignature_NilBundle(t *testing.T) {
	// Defensive: never panic on a nil bundle even though callers shouldn't
	// hit this path.
	injectCentralSignature(nil, http.Header{})
}
