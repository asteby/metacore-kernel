// Package sigv2 is the generic, embedder-agnostic signing base of the metacore
// trust chain, version 2. It adds, WITHOUT changing anything that exists:
//
//   - the "asteby1." signature envelope: a token that names the signing key
//     (kid) and the message type (dom, a domain-separation tag), so a signature
//     minted for one purpose can never be replayed as another one;
//   - a TUF-style key set ("root.json"): keys per role, threshold, expiry and a
//     strictly monotonic version, rotated by chaining (root N+1 must be signed
//     by root N), so keys can be rotated without re-flashing the binary;
//   - signed state lists (revocations) with a sequence number and an expiry,
//     plus a Tracker that rejects old or expired lists and applies a
//     configurable policy when no fresh list is available.
//
// COMPATIBILITY. The legacy token format (<b64(claims)>.<b64(sig)>, no kid, no
// domain) keeps verifying: it is treated as signed by the key with kid
// LegacyKID. Policy.RequireV2 turns the legacy format off (downgrade
// protection) once an embedder is ready. Nothing here issues anything by
// itself: issuance stays in the hub, behind its own flag.
//
// WIRE FORMAT of the envelope:
//
//	token   = "asteby1." + b64u(header) + "." + b64u(payload) + "." + b64u(sig)
//	header  = {"kid":"<key id>","dom":"<domain tag>","alg":"Ed25519"}
//	message = utf8(dom) || 0x0A || header_bytes || "." || payload_bytes
//
// Verification uses the header and payload bytes exactly as received (never a
// re-marshal), and the dom signed is the one INSIDE the header, which the
// verifier then compares with the dom it expects: a lease token presented where
// a revocation list is expected fails with ErrWrongDomain.
package sigv2

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Prefix identifies a v2 envelope. A legacy token's first segment is the
// base64url of a JSON object (it starts with "eyJ"), so it never collides.
const Prefix = "asteby1."

// Domain tags: one per message type. Add new ones, never reuse.
const (
	DomGrant       = "asteby/grant/v2"       // commercial license / grant
	DomLease       = "asteby/lease/v2"       // per-instance operating permit (renewed)
	DomRelease     = "asteby/release/v2"     // release manifests
	DomRevocations = "asteby/revocations/v2" // revocation lists
	DomState       = "asteby/state/v2"       // other signed state documents
)

// LegacyKID is the kid under which a token without a key id is verified.
const LegacyKID = "legacy"

const algEd25519 = "Ed25519"

// maxTokenBytes bounds what Open will parse.
const maxTokenBytes = 1 << 20

// Errors callers branch on.
var (
	ErrMalformed    = errors.New("sigv2: token is malformed")
	ErrBadSig       = errors.New("sigv2: signature does not verify")
	ErrUnknownKID   = errors.New("sigv2: unknown key id")
	ErrWrongDomain  = errors.New("sigv2: token was signed for a different message type")
	ErrKeyRole      = errors.New("sigv2: key is not allowed to sign this message type")
	ErrDowngrade    = errors.New("sigv2: legacy (key-less) format refused: v2 envelope required")
	ErrUnsupported  = errors.New("sigv2: unsupported algorithm")
	ErrKeyNotActive = errors.New("sigv2: key is outside its validity window")
)

var enc = base64.RawURLEncoding

// RoleForDomain maps a domain tag to the key-set role allowed to sign it. An
// unknown domain maps to "" (no key-set role can sign it).
func RoleForDomain(dom string) string {
	switch dom {
	case DomGrant:
		return "grant"
	case DomLease:
		return "lease"
	case DomRelease:
		return "release"
	case DomRevocations, DomState:
		return "status"
	}
	return ""
}

// KeyID derives the stable key id of a public key: the first 8 bytes of
// sha256(pub), base32 (lowercase, no padding).
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:8]))
}

// Keyring resolves the public key for (kid, dom) at `now`. It returns
// ErrUnknownKID for a key it does not know and ErrKeyRole when the key exists
// but may not sign that message type.
type Keyring interface {
	Resolve(kid, dom string, now time.Time) (ed25519.PublicKey, error)
}

// StaticKeyring is a fixed set of keys; the zero value is an empty ring.
type StaticKeyring struct {
	keys map[string]staticKey
}

type staticKey struct {
	pub   ed25519.PublicKey
	roles map[string]bool // empty = any role
}

// NewStaticKeyring returns an empty ring.
func NewStaticKeyring() *StaticKeyring { return &StaticKeyring{keys: map[string]staticKey{}} }

// Add registers pub under kid, allowed to sign only the given roles (none =
// any role).
func (r *StaticKeyring) Add(kid string, pub ed25519.PublicKey, roles ...string) *StaticKeyring {
	if r.keys == nil {
		r.keys = map[string]staticKey{}
	}
	m := map[string]bool{}
	for _, ro := range roles {
		m[ro] = true
	}
	r.keys[kid] = staticKey{pub: pub, roles: m}
	return r
}

// AddLegacy registers the pre-v2 key: it answers to LegacyKID and to its
// derived KeyID, and may sign any message type (it signed everything before).
func (r *StaticKeyring) AddLegacy(pub ed25519.PublicKey) *StaticKeyring {
	r.Add(LegacyKID, pub)
	return r.Add(KeyID(pub), pub)
}

// Resolve implements Keyring.
func (r *StaticKeyring) Resolve(kid, dom string, _ time.Time) (ed25519.PublicKey, error) {
	k, ok := r.keys[kid]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKID, kid)
	}
	if len(k.roles) > 0 && !k.roles[RoleForDomain(dom)] {
		return nil, fmt.Errorf("%w (%s)", ErrKeyRole, dom)
	}
	return k.pub, nil
}

// Rings tries each ring in order; the first that knows the kid decides. An
// ErrKeyRole from a ring that knows the key is final (does not fall through).
type Rings []Keyring

// Resolve implements Keyring.
func (rs Rings) Resolve(kid, dom string, now time.Time) (ed25519.PublicKey, error) {
	var last error = fmt.Errorf("%w %q", ErrUnknownKID, kid)
	for _, r := range rs {
		if r == nil {
			continue
		}
		pub, err := r.Resolve(kid, dom, now)
		if err == nil {
			return pub, nil
		}
		if !errors.Is(err, ErrUnknownKID) {
			return nil, err
		}
		last = err
	}
	return nil, last
}

type header struct {
	KID string `json:"kid"`
	Dom string `json:"dom"`
	Alg string `json:"alg"`
}

func signedMessage(dom string, headerBytes, payload []byte) []byte {
	msg := make([]byte, 0, len(dom)+len(headerBytes)+len(payload)+2)
	msg = append(msg, dom...)
	msg = append(msg, 0x0A)
	msg = append(msg, headerBytes...)
	msg = append(msg, '.')
	msg = append(msg, payload...)
	return msg
}

// Seal signs payload as a v2 envelope under kid and domain dom.
func Seal(priv ed25519.PrivateKey, kid, dom string, payload []byte) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("sigv2: invalid Ed25519 private key")
	}
	if kid == "" || dom == "" {
		return "", errors.New("sigv2: kid and dom are required")
	}
	hb, err := json.Marshal(header{KID: kid, Dom: dom, Alg: algEd25519})
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, signedMessage(dom, hb, payload))
	return Prefix + enc.EncodeToString(hb) + "." + enc.EncodeToString(payload) + "." + enc.EncodeToString(sig), nil
}

// IsEnvelope reports whether token is in the v2 format.
func IsEnvelope(token string) bool { return strings.HasPrefix(strings.TrimSpace(token), Prefix) }

// Opened is a verified token.
type Opened struct {
	KID     string
	Dom     string
	Payload []byte
	// Legacy is true when the token was in the key-less pre-v2 format.
	Legacy bool
}

// Policy tunes Open.
type Policy struct {
	// RequireV2 refuses the legacy key-less format (downgrade protection).
	RequireV2 bool
}

// Open verifies token against keys and returns its payload. It accepts BOTH
// formats: a v2 envelope (domain must equal wantDom) and, unless
// pol.RequireV2, the legacy two-part token, verified with LegacyKID. It does
// not look at any validity window: that is the payload's business.
func Open(token, wantDom string, keys Keyring, pol Policy, now time.Time) (*Opened, error) {
	token = strings.TrimSpace(token)
	if len(token) == 0 || len(token) > maxTokenBytes {
		return nil, fmt.Errorf("%w: empty or oversized", ErrMalformed)
	}
	if !IsEnvelope(token) {
		return openLegacy(token, wantDom, keys, pol, now)
	}
	parts := strings.Split(strings.TrimPrefix(token, Prefix), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: expected %s<header>.<payload>.<sig>", ErrMalformed, Prefix)
	}
	hb, err := enc.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header not base64url", ErrMalformed)
	}
	payload, err := enc.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload not base64url", ErrMalformed)
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature not %d-byte base64url", ErrMalformed, ed25519.SignatureSize)
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil || h.KID == "" || h.Dom == "" {
		return nil, fmt.Errorf("%w: bad header", ErrMalformed)
	}
	if h.Alg != algEd25519 {
		return nil, fmt.Errorf("%w %q", ErrUnsupported, h.Alg)
	}
	// Domain first: a wrong-type token is refused before any key lookup, and
	// the dom that was signed is the one in the header, so it cannot be spoofed.
	if h.Dom != wantDom {
		return nil, fmt.Errorf("%w (got %q, want %q)", ErrWrongDomain, h.Dom, wantDom)
	}
	pub, err := keys.Resolve(h.KID, h.Dom, now)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(pub, signedMessage(h.Dom, hb, payload), sig) {
		return nil, ErrBadSig
	}
	return &Opened{KID: h.KID, Dom: h.Dom, Payload: payload}, nil
}

// openLegacy verifies the pre-v2 token: <b64(payload)>.<b64(sig)>, signature
// over the raw payload bytes, no domain, key = LegacyKID.
func openLegacy(token, wantDom string, keys Keyring, pol Policy, now time.Time) (*Opened, error) {
	if pol.RequireV2 {
		return nil, ErrDowngrade
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: expected <claims>.<sig>", ErrMalformed)
	}
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: claims not base64url", ErrMalformed)
	}
	sig, err := enc.DecodeString(parts[1])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature not %d-byte base64url", ErrMalformed, ed25519.SignatureSize)
	}
	pub, err := keys.Resolve(LegacyKID, wantDom, now)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, ErrBadSig
	}
	return &Opened{KID: LegacyKID, Dom: wantDom, Payload: payload, Legacy: true}, nil
}
