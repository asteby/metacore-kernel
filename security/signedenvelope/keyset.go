package signedenvelope

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/licensing/sigv2"
)

// Purpose is a domain-separation tag: one per kind of signed message. Add new
// ones, never reuse or repurpose an existing one.
type Purpose string

// Native purposes. The first five are the sigv2 domains (already deployed);
// the rest are the purposes the ops/hub migration needs, in the same naming
// scheme. A purpose must be non-empty, printable ASCII, without newlines.
const (
	PurposeGrant       Purpose = sigv2.DomGrant       // commercial grant / licence
	PurposeLease       Purpose = sigv2.DomLease       // per-instance lease
	PurposeRelease     Purpose = sigv2.DomRelease     // release manifests / binaries
	PurposeRevocations Purpose = sigv2.DomRevocations // revocation lists
	PurposeState       Purpose = sigv2.DomState       // other signed state
	PurposeBundle      Purpose = "asteby/bundle/v2"   // addon bundle digest
	PurposeOrder       Purpose = "asteby/order/v2"    // hub orders
	PurposeBootstrap   Purpose = "asteby/act-bootstrap/v2"
	PurposeBrandPack   Purpose = "asteby/brandpack/v2"
	PurposeHubControl  Purpose = "asteby/hub-control/v2"
	PurposeEnter       Purpose = "asteby/enter/v2" // hub -> instance entry token
	PurposeEdgeUpdate  Purpose = "asteby/edge-update/v2"
)

func (p Purpose) valid() bool {
	if p == "" || len(p) > 128 {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// Key is one trusted verification key.
type Key struct {
	// ID is the key id (kid). Empty derives it from Public (sigv2.KeyID).
	ID     string
	Public ed25519.PublicKey
	// Purposes lists what this key may sign. REQUIRED and non-empty: a key
	// with no declared purpose would be a skeleton key, so it is refused.
	Purposes []Purpose
	// NotBefore/NotAfter bound the key's validity at verification time. nil =
	// unbounded on that side.
	NotBefore *time.Time
	NotAfter  *time.Time
	// Revoked keys never verify (kept in the set so a known kid yields a
	// precise error instead of "unknown key").
	Revoked bool
}

// KeySet is an immutable collection of keys. Build it with NewKeySet.
type KeySet struct {
	byID  map[string]Key
	order []string // insertion order, deterministic iteration
}

// NewKeySet validates and indexes keys.
func NewKeySet(keys ...Key) (*KeySet, error) {
	ks := &KeySet{byID: map[string]Key{}}
	for i, k := range keys {
		if len(k.Public) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: key %d has %d bytes, want %d", ErrInvalidConfig, i, len(k.Public), ed25519.PublicKeySize)
		}
		if k.ID == "" {
			k.ID = sigv2.KeyID(k.Public)
		}
		if _, dup := ks.byID[k.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate key id %q", ErrInvalidConfig, k.ID)
		}
		if len(k.Purposes) == 0 {
			return nil, fmt.Errorf("%w: key %q declares no purposes", ErrInvalidConfig, k.ID)
		}
		for _, p := range k.Purposes {
			if !p.valid() {
				return nil, fmt.Errorf("%w: key %q has an invalid purpose %q", ErrInvalidConfig, k.ID, string(p))
			}
		}
		if k.NotBefore != nil && k.NotAfter != nil && !k.NotAfter.After(*k.NotBefore) {
			return nil, fmt.Errorf("%w: key %q has an empty validity window", ErrInvalidConfig, k.ID)
		}
		k.Public = append(ed25519.PublicKey(nil), k.Public...)
		k.Purposes = append([]Purpose(nil), k.Purposes...)
		ks.byID[k.ID] = k
		ks.order = append(ks.order, k.ID)
	}
	return ks, nil
}

// ParseHexKeys builds Keys from a comma-separated list of hex Ed25519 public
// keys (the MARKETPLACE_PUBKEY(S) convention), all allowed for purposes. IDs
// are derived. Empty list yields no keys and no error (callers decide whether
// "no key configured" is fatal: NewKeySet + Verifier fail closed anyway).
func ParseHexKeys(csv string, purposes ...Purpose) ([]Key, error) {
	var out []Key
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		raw, err := hex.DecodeString(part)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: public key is not %d-byte hex", ErrInvalidConfig, ed25519.PublicKeySize)
		}
		out = append(out, Key{Public: raw, Purposes: purposes})
	}
	return out, nil
}

func (k Key) allows(p Purpose) bool {
	for _, q := range k.Purposes {
		if q == p {
			return true
		}
	}
	return false
}

func (k Key) activeAt(now time.Time) bool {
	if k.Revoked {
		return false
	}
	if k.NotBefore != nil && now.Before(*k.NotBefore) {
		return false
	}
	if k.NotAfter != nil && !now.Before(*k.NotAfter) {
		return false
	}
	return true
}

// Resolve implements sigv2.Keyring so the native envelope verifier can use the
// set directly. dom is the purpose the envelope claims.
func (ks *KeySet) Resolve(kid, dom string, now time.Time) (ed25519.PublicKey, error) {
	k, ok := ks.byID[kid]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKey, kid)
	}
	if !k.allows(Purpose(dom)) {
		return nil, fmt.Errorf("%w (%s)", ErrKeyPurpose, dom)
	}
	if !k.activeAt(now) {
		return nil, fmt.Errorf("%w (%s)", ErrKeyNotActive, kid)
	}
	return k.Public, nil
}

// candidates returns the keys usable for purpose at now, in insertion order,
// plus the reason none qualified (for precise errors).
func (ks *KeySet) candidates(p Purpose, now time.Time) ([]Key, error) {
	var out []Key
	sawPurpose, sawActive := false, false
	for _, id := range ks.order {
		k := ks.byID[id]
		if !k.allows(p) {
			continue
		}
		sawPurpose = true
		if !k.activeAt(now) {
			continue
		}
		sawActive = true
		out = append(out, k)
	}
	switch {
	case !sawPurpose:
		return nil, fmt.Errorf("%w: %s", ErrNoKeys, p)
	case !sawActive:
		return nil, fmt.Errorf("%w: every key for %s", ErrKeyNotActive, p)
	}
	return out, nil
}
