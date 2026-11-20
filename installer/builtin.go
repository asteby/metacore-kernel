package installer

// SECURITY-SENSITIVE — builtin trust path. Review before merging.
//
// A module compiled into the host binary ships its manifest, migrations,
// locales and templates as a bundle embedded in that same binary. It is not in
// the marketplace, so it has no hub signature and no entry in the hub's
// revocation list. The signed path (verifySignature) cannot admit it and must
// not be weakened to do so. This file adds a SEPARATE, narrow trust path:
//
//   - trust is an exact digest, not a key and not a flag: the host registers
//     `key@version -> sha256(bundle bytes)` at boot with TrustBuiltin, computing
//     the digest from the embedded bytes (BuiltinDigest). A bundle is admitted
//     only if sha256(b.Raw) equals the registered digest AND b.Raw really is the
//     bundle b describes (re-read and compared), so a hand-built *bundle.Bundle
//     cannot pair a trusted Raw with a different manifest;
//   - it is reachable ONLY through InstallBuiltin / UpgradeBuiltin, which the
//     host calls with embedded bytes. Install / Upgrade (the marketplace and
//     sideload paths) never consult the registry, so a marketplace bundle can
//     never be admitted by it, whatever digest it carries;
//   - no environment variable or config enables it, and an empty registry
//     admits nothing (fail-closed);
//   - the installation row records Source="builtin"; a builtin row is not
//     touched by the signed paths and a signed/sideloaded row is not taken over
//     by the builtin ones (ErrBuiltinSourceMismatch);
//   - signature verification and the hub revocation check are skipped ONLY for
//     the builtin path (the hub does not know these modules); every other
//     install step (manifest validation, runtime/capability checks, compiled
//     handler gate, migrations, compensation) is the same code as the signed
//     path.

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/bundle"
)

// SourceBuiltin is Installation.Source of a module installed through the
// builtin trust path.
const SourceBuiltin = "builtin"

// trustMode selects how an install/upgrade is authenticated.
type trustMode int

const (
	trustSigned  trustMode = iota // signature + revocation (marketplace, sideload)
	trustBuiltin                  // exact digest of the embedded bundle
)

// Errors of the builtin path.
var (
	// ErrBuiltinNotTrusted: no digest is registered for key@version, the digest
	// does not match the bundle bytes, or the bundle is not what its Raw says.
	ErrBuiltinNotTrusted = errors.New("installer: bundle is not a trusted builtin")
	// ErrBuiltinSourceMismatch: the installation row was created by the other
	// trust path; neither path takes the other's installations over.
	ErrBuiltinSourceMismatch = errors.New("installer: installation belongs to a different trust path (builtin vs signed)")
)

// BuiltinDigest returns the hex sha256 of the bundle bytes (the tar.gz a
// bundle.Read consumed), the value TrustBuiltin expects.
func BuiltinDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TrustBuiltin registers the exact digest of the embedded bundle for
// key@version. Call it at boot, from bytes embedded in the binary, before
// InstallBuiltin/UpgradeBuiltin. Registering the same key@version with a
// different digest is refused (a binary has one bundle per version).
func (i *Installer) TrustBuiltin(addonKey, version, digestHex string) error {
	addonKey, version = strings.TrimSpace(addonKey), strings.TrimSpace(version)
	digestHex = strings.ToLower(strings.TrimSpace(digestHex))
	if addonKey == "" || version == "" {
		return errors.New("installer: TrustBuiltin: addon key and version are required")
	}
	if raw, err := hex.DecodeString(digestHex); err != nil || len(raw) != sha256.Size {
		return errors.New("installer: TrustBuiltin: digest must be a hex sha256")
	}
	i.builtinMu.Lock()
	defer i.builtinMu.Unlock()
	k := builtinKey(addonKey, version)
	if prev, ok := i.builtinDigests[k]; ok && prev != digestHex {
		return fmt.Errorf("installer: TrustBuiltin: %s already trusted with a different digest", k)
	}
	if i.builtinDigests == nil {
		i.builtinDigests = map[string]string{}
	}
	i.builtinDigests[k] = digestHex
	return nil
}

func builtinKey(addonKey, version string) string { return addonKey + "@" + version }

// verifyBuiltin admits b only when it is byte-for-byte the bundle whose digest
// the host registered for its key@version.
func (i *Installer) verifyBuiltin(b *bundle.Bundle) error {
	if b == nil {
		return fmt.Errorf("%w: nil bundle", ErrBuiltinNotTrusted)
	}
	if len(b.Raw) == 0 {
		return fmt.Errorf("%w: bundle has no Raw bytes (build it with bundle.Read)", ErrBuiltinNotTrusted)
	}
	i.builtinMu.RLock()
	want, ok := i.builtinDigests[builtinKey(b.Manifest.Key, b.Manifest.Version)]
	i.builtinMu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrBuiltinNotTrusted, builtinKey(b.Manifest.Key, b.Manifest.Version))
	}
	got := BuiltinDigest(b.Raw)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("%w: digest of %s does not match", ErrBuiltinNotTrusted, builtinKey(b.Manifest.Key, b.Manifest.Version))
	}
	// The digest vouches for Raw, not for the struct: re-read Raw and require
	// the struct to describe the same bundle.
	again, err := bundle.Read(bytes.NewReader(b.Raw), int64(len(b.Raw))+1)
	if err != nil {
		return fmt.Errorf("%w: re-reading bundle: %v", ErrBuiltinNotTrusted, err)
	}
	if again.Manifest.Key != b.Manifest.Key || again.Manifest.Version != b.Manifest.Version ||
		!bytes.Equal(again.RawManifest, b.RawManifest) {
		return fmt.Errorf("%w: bundle does not match its own bytes", ErrBuiltinNotTrusted)
	}
	return nil
}

// verifyTrust authenticates b for mode. The signed mode is exactly the previous
// behaviour (signature, then revocation); the builtin mode checks only the exact
// digest.
func (i *Installer) verifyTrust(b *bundle.Bundle, mode trustMode, checkTail func() error) error {
	if mode == trustBuiltin {
		return i.verifyBuiltin(b)
	}
	if err := i.verifySignature(b); err != nil {
		return err
	}
	return checkTail()
}

func sourceFor(b *bundle.Bundle, mode trustMode) string {
	if mode == trustBuiltin {
		return SourceBuiltin
	}
	return sourceOf(b.Manifest)
}

// checkSourceTrust refuses to cross the two trust paths on an existing row.
func checkSourceTrust(prev *Installation, mode trustMode) error {
	if prev == nil || prev.Status == "failed" {
		return nil
	}
	if (prev.Source == SourceBuiltin) != (mode == trustBuiltin) {
		return ErrBuiltinSourceMismatch
	}
	return nil
}
