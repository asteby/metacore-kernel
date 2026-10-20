package sigv2

// keyset.go: a TUF-style key set ("root.json"). It declares which keys may
// sign which role, with a threshold, a validity window per key, an expiry and a
// strictly monotonic version. A new root is accepted only if it is signed by
// the CURRENT root's "root" role (threshold) and by its OWN "root" role: that
// chain is what lets keys rotate without re-flashing the binary that pins the
// first root.
//
// This file contains verification only. There is no ceremony and no real key in
// this repository: tests generate throwaway keys on the fly.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// RootDomain tags the bytes a root signature covers.
const RootDomain = "asteby/root/v1"

// RootRole is the role whose keys sign root documents.
const RootRole = "root"

var (
	ErrRootMalformed   = errors.New("sigv2: root document is malformed")
	ErrRootExpired     = errors.New("sigv2: root document has expired")
	ErrRootThreshold   = errors.New("sigv2: root signatures below threshold")
	ErrRootRollback    = errors.New("sigv2: root version is not the next version")
	ErrRootNotBootable = errors.New("sigv2: pinned root is not self-consistent")
)

// KeyEntry is one key of the set.
type KeyEntry struct {
	// Pub is the Ed25519 public key, hex.
	Pub       string     `json:"pub"`
	NotBefore *time.Time `json:"not_before,omitempty"`
	NotAfter  *time.Time `json:"not_after,omitempty"`
}

// RoleEntry lists the keys of a role and how many must sign.
type RoleEntry struct {
	KeyIDs    []string `json:"keyids"`
	Threshold int      `json:"threshold"`
}

// RootDoc is the signed part of root.json.
type RootDoc struct {
	Type    string               `json:"_type"` // "root"
	Version int                  `json:"version"`
	Expires time.Time            `json:"expires"`
	Keys    map[string]KeyEntry  `json:"keys"`
	Roles   map[string]RoleEntry `json:"roles"`
}

// RootSig is one signature over the signed bytes.
type RootSig struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"` // base64url
}

// SignedRoot is root.json on the wire. Signed is kept as raw bytes: signatures
// cover exactly those bytes, never a re-marshal.
type SignedRoot struct {
	Signed     json.RawMessage `json:"signed"`
	Signatures []RootSig       `json:"signatures"`
}

func rootMessage(signed []byte) []byte {
	m := append([]byte(RootDomain), 0x0A)
	return append(m, signed...)
}

// SignRoot builds a SignedRoot from doc signed by the given keys (kid ->
// private key). It exists for the future ceremony tool and for tests.
func SignRoot(doc RootDoc, signers map[string]ed25519.PrivateKey) ([]byte, error) {
	signed, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	sr := SignedRoot{Signed: signed}
	for kid, priv := range signers {
		sr.Signatures = append(sr.Signatures, RootSig{
			KeyID: kid,
			Sig:   enc.EncodeToString(ed25519.Sign(priv, rootMessage(signed))),
		})
	}
	return json.Marshal(sr)
}

func parseRoot(raw []byte) (*SignedRoot, *RootDoc, error) {
	var sr SignedRoot
	if err := json.Unmarshal(raw, &sr); err != nil || len(sr.Signed) == 0 {
		return nil, nil, fmt.Errorf("%w: envelope", ErrRootMalformed)
	}
	var doc RootDoc
	if err := json.Unmarshal(sr.Signed, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrRootMalformed, err)
	}
	if doc.Type != "root" || doc.Version < 1 || doc.Expires.IsZero() {
		return nil, nil, fmt.Errorf("%w: type/version/expires", ErrRootMalformed)
	}
	if re, ok := doc.Roles[RootRole]; !ok || re.Threshold < 1 || len(re.KeyIDs) < re.Threshold {
		return nil, nil, fmt.Errorf("%w: root role", ErrRootMalformed)
	}
	for role, re := range doc.Roles {
		if re.Threshold < 1 {
			return nil, nil, fmt.Errorf("%w: role %q threshold < 1", ErrRootMalformed, role)
		}
		for _, kid := range re.KeyIDs {
			ke, ok := doc.Keys[kid]
			if !ok {
				return nil, nil, fmt.Errorf("%w: role %q names unknown key %q", ErrRootMalformed, role, kid)
			}
			if b, err := hex.DecodeString(ke.Pub); err != nil || len(b) != ed25519.PublicKeySize {
				return nil, nil, fmt.Errorf("%w: key %q is not ed25519 hex", ErrRootMalformed, kid)
			}
		}
	}
	return &sr, &doc, nil
}

// meetsThreshold counts distinct valid signatures from the role's keys in `by`.
func meetsThreshold(sr *SignedRoot, by *RootDoc, role string, now time.Time) bool {
	re, ok := by.Roles[role]
	if !ok {
		return false
	}
	allowed := map[string]bool{}
	for _, k := range re.KeyIDs {
		allowed[k] = true
	}
	seen := map[string]bool{}
	for _, s := range sr.Signatures {
		if !allowed[s.KeyID] || seen[s.KeyID] {
			continue
		}
		ke := by.Keys[s.KeyID]
		if !keyActive(ke, now) {
			continue
		}
		pub, err := hex.DecodeString(ke.Pub)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		sig, err := enc.DecodeString(s.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(pub), rootMessage(sr.Signed), sig) {
			seen[s.KeyID] = true
		}
	}
	return len(seen) >= re.Threshold
}

func keyActive(k KeyEntry, now time.Time) bool {
	if k.NotBefore != nil && now.Before(*k.NotBefore) {
		return false
	}
	if k.NotAfter != nil && now.After(*k.NotAfter) {
		return false
	}
	return true
}

// KeySet is the current trusted root plus the chain-update logic. Safe for
// concurrent use. It implements Keyring.
type KeySet struct {
	mu  sync.RWMutex
	doc *RootDoc
}

// Bootstrap trusts a PINNED root (the one baked into the binary). It must be
// signed by its own root role and not be expired at `now`.
func Bootstrap(raw []byte, now time.Time) (*KeySet, error) {
	sr, doc, err := parseRoot(raw)
	if err != nil {
		return nil, err
	}
	if !meetsThreshold(sr, doc, RootRole, now) {
		return nil, fmt.Errorf("%w: %w", ErrRootNotBootable, ErrRootThreshold)
	}
	if !now.Before(doc.Expires) {
		return nil, ErrRootExpired
	}
	return &KeySet{doc: doc}, nil
}

// Version is the version of the root currently trusted.
func (ks *KeySet) Version() int {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.doc.Version
}

// Expired reports whether the trusted root is past its expiry at now. Resolve
// does NOT refuse on expiry (an offline box keeps working with the last root it
// verified); the embedder reads this to warn or to apply its own policy.
func (ks *KeySet) Expired(now time.Time) bool {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return !now.Before(ks.doc.Expires)
}

// Update chains to the next root. It accepts only version == current+1, signed
// by the current root's "root" role AND by the new root's own "root" role, and
// not expired. On any error the trusted root is unchanged.
func (ks *KeySet) Update(raw []byte, now time.Time) error {
	sr, next, err := parseRoot(raw)
	if err != nil {
		return err
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if next.Version != ks.doc.Version+1 {
		return fmt.Errorf("%w (have %d, got %d)", ErrRootRollback, ks.doc.Version, next.Version)
	}
	if !now.Before(next.Expires) {
		return ErrRootExpired
	}
	if !meetsThreshold(sr, ks.doc, RootRole, now) {
		return fmt.Errorf("%w: not signed by the current root", ErrRootThreshold)
	}
	if !meetsThreshold(sr, next, RootRole, now) {
		return fmt.Errorf("%w: not signed by the new root", ErrRootThreshold)
	}
	ks.doc = next
	return nil
}

// Resolve implements Keyring: kid must belong to the role that owns dom, and be
// inside its validity window.
func (ks *KeySet) Resolve(kid, dom string, now time.Time) (ed25519.PublicKey, error) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	ke, ok := ks.doc.Keys[kid]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKID, kid)
	}
	role := RoleForDomain(dom)
	re, ok := ks.doc.Roles[role]
	if role == "" || !ok {
		return nil, fmt.Errorf("%w (%s)", ErrKeyRole, dom)
	}
	listed := false
	for _, k := range re.KeyIDs {
		if k == kid {
			listed = true
			break
		}
	}
	if !listed {
		return nil, fmt.Errorf("%w (%s)", ErrKeyRole, dom)
	}
	if !keyActive(ke, now) {
		return nil, ErrKeyNotActive
	}
	b, err := hex.DecodeString(ke.Pub)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrRootMalformed
	}
	return ed25519.PublicKey(b), nil
}
