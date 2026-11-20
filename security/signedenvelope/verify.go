package signedenvelope

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/asteby/metacore-kernel/licensing/sigv2"
)

// Window is the optional validity window carried by a payload.
type Window struct {
	NotBefore *time.Time
	Expires   *time.Time
}

// WindowFunc extracts the window from a verified payload. It runs only AFTER
// the signature verified, so it never parses attacker-controlled bytes.
type WindowFunc func(payload []byte) (Window, error)

// windowFromFields builds a WindowFunc from a field reader. A field name that
// is empty is "not used". When required is true every named field MUST be
// present and non-null/non-zero: an absent field is ErrMalformed, never
// "unbounded" (an issuer bug or a stripped field must not turn into a licence
// that never expires).
func windowFromFields(nbfField, expField string, required bool, parse func(raw json.RawMessage) (time.Time, error)) WindowFunc {
	return func(payload []byte) (Window, error) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(payload, &m); err != nil {
			return Window{}, fmt.Errorf("%w: payload is not a JSON object", ErrMalformed)
		}
		get := func(f string) (*time.Time, error) {
			if f == "" {
				return nil, nil
			}
			raw, ok := m[f]
			if !ok || string(raw) == "null" {
				if required {
					return nil, fmt.Errorf("%w: required field %q is missing", ErrMalformed, f)
				}
				return nil, nil
			}
			t, err := parse(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: field %q: %v", ErrMalformed, f, err)
			}
			if required && t.IsZero() {
				return nil, fmt.Errorf("%w: required field %q is zero", ErrMalformed, f)
			}
			return &t, nil
		}
		var w Window
		var err error
		if w.NotBefore, err = get(nbfField); err != nil {
			return Window{}, err
		}
		if w.Expires, err = get(expField); err != nil {
			return Window{}, err
		}
		return w, nil
	}
}

// UnixWindow reads a window from a JSON object whose fields hold integer unix
// seconds. Use it only for payloads that really carry unix seconds (many
// deployed licences do NOT: see RFC3339Window). required: see windowFromFields.
func UnixWindow(nbfField, expField string, required bool) WindowFunc {
	return windowFromFields(nbfField, expField, required, func(raw json.RawMessage) (time.Time, error) {
		var sec int64
		if err := json.Unmarshal(raw, &sec); err != nil {
			return time.Time{}, errors.New("not unix seconds")
		}
		return time.Unix(sec, 0).UTC(), nil
	})
}

// RFC3339Window reads a window from a JSON object whose fields are RFC3339
// strings, which is what Go's encoding/json writes for time.Time. This is the
// real shape of the deployed instance licences (hub InstanceClaims: "iat" /
// "exp") and of the ops application licence ("issued_at" / "expires_at").
// required: see windowFromFields.
func RFC3339Window(nbfField, expField string, required bool) WindowFunc {
	return windowFromFields(nbfField, expField, required, func(raw json.RawMessage) (time.Time, error) {
		var t time.Time
		if err := json.Unmarshal(raw, &t); err != nil {
			return time.Time{}, errors.New("not an RFC3339 time")
		}
		return t.UTC(), nil
	})
}

// Policy tunes a Verifier.
type Policy struct {
	// RequireNative refuses every legacy format (downgrade protection). It is
	// the end state of a migration; flip it per purpose with a dedicated
	// Verifier once the issuer emits "asteby1." tokens.
	RequireNative bool
	// Window, if set, is applied to the payload of Open results.
	Window WindowFunc
	// Leeway widens the payload window AND key validity windows on both sides
	// (clock skew). It must not be negative.
	Leeway time.Duration
}

// Verifier verifies material against a KeySet. The key set can be swapped
// atomically at runtime (SetKeys), e.g. to revoke a key without a restart.
type Verifier struct {
	keys   atomic.Pointer[KeySet]
	policy Policy
	now    func() time.Time
}

// NewVerifier builds a Verifier. keys must be non-nil.
func NewVerifier(keys *KeySet, p Policy) (*Verifier, error) {
	if keys == nil {
		return nil, fmt.Errorf("%w: nil key set", ErrInvalidConfig)
	}
	if p.Leeway < 0 {
		return nil, fmt.Errorf("%w: negative leeway", ErrInvalidConfig)
	}
	v := &Verifier{policy: p, now: time.Now}
	v.keys.Store(keys)
	return v, nil
}

// SetKeys atomically replaces the key set (nil is ignored). In-flight
// verifications finish against the set they started with.
func (v *Verifier) SetKeys(ks *KeySet) {
	if ks != nil {
		v.keys.Store(ks)
	}
}

// Keys returns the current (immutable) key set.
func (v *Verifier) Keys() *KeySet { return v.keys.Load() }

// WithClock returns v after setting its clock (tests).
func (v *Verifier) WithClock(now func() time.Time) *Verifier { v.now = now; return v }

// Result describes a successful verification.
type Result struct {
	KeyID   string // kid for native tokens; sigv2.LegacyKID for the legacy token
	Purpose Purpose
	Payload []byte // the verified payload (Open); nil for VerifyLegacy
	Legacy  bool   // true when a pre-native format was accepted
	Scheme  string // legacy scheme name, "" for native
	Window  Window
}

func (v *Verifier) clock() time.Time {
	if v.now != nil {
		return v.now().UTC()
	}
	return time.Now().UTC()
}

// Seal signs payload as a native envelope under kid and purpose. Thin wrapper
// over sigv2.Seal, here so issuers and tests do not need a second import.
func Seal(priv ed25519.PrivateKey, kid string, p Purpose, payload []byte) (string, error) {
	if !p.valid() {
		return "", fmt.Errorf("%w: invalid purpose", ErrInvalidConfig)
	}
	return sigv2.Seal(priv, kid, string(p), payload)
}

// Open verifies token for purpose want. It accepts the native "asteby1."
// envelope and, unless Policy.RequireNative, the legacy "<b64 payload>.<b64
// sig>" token (the legacy token carries no kid, so every active key declared
// for want is a candidate).
//
// WINDOW FAILURES RETURN THE PAYLOAD. When the signature is valid but the
// payload window fails (ErrExpired, ErrNotYetValid), Open returns BOTH a
// non-nil *Result (signature authenticated, Payload and Window populated) and
// the error, so callers that implement a grace period (applicense, instance
// licences) can compute it from authenticated data. Callers must check err
// first and treat a non-nil Result alongside an error as "authentic but not
// currently valid" only for those two errors; for every other error the
// Result is nil.
func (v *Verifier) Open(want Purpose, token string) (*Result, error) {
	if v == nil || v.Keys() == nil {
		return nil, fmt.Errorf("%w: no key set", ErrInvalidConfig)
	}
	ks := v.Keys()
	if !want.valid() {
		return nil, fmt.Errorf("%w: invalid purpose", ErrInvalidConfig)
	}
	now := v.clock()
	lee := v.policy.Leeway
	var res *Result
	if sigv2.IsEnvelope(token) {
		op, err := sigv2.Open(token, string(want), ring{ks, lee}, sigv2.Policy{RequireV2: true}, now)
		if err != nil {
			return nil, mapV2(err)
		}
		res = &Result{KeyID: op.KID, Purpose: want, Payload: op.Payload}
	} else {
		if v.policy.RequireNative {
			return nil, ErrDowngrade
		}
		cands, err := ks.candidates(want, now, lee)
		if err != nil {
			return nil, err
		}
		// One sigv2 call per candidate through a one-key ring: sigv2 owns the
		// legacy token parsing and the signature check.
		var last error = ErrBadSignature
		for _, k := range cands {
			one := sigv2.NewStaticKeyring().Add(sigv2.LegacyKID, k.Public)
			op, err := sigv2.Open(token, string(want), one, sigv2.Policy{}, now)
			if err == nil {
				res = &Result{KeyID: k.ID, Purpose: want, Payload: op.Payload, Legacy: true, Scheme: "token2"}
				break
			}
			last = mapV2(err)
			if !errors.Is(last, ErrBadSignature) {
				return nil, last // malformed etc: no point trying other keys
			}
		}
		if res == nil {
			return nil, last
		}
	}
	if v.policy.Window != nil {
		w, err := v.policy.Window(res.Payload)
		if err != nil {
			return nil, err
		}
		res.Window = w
		if w.NotBefore != nil && now.Add(lee).Before(*w.NotBefore) {
			return res, ErrNotYetValid
		}
		if w.Expires != nil && !now.Add(-lee).Before(*w.Expires) {
			return res, ErrExpired
		}
	}
	return res, nil
}

// VerifyDetached checks sig over msg with one key and the strict size checks
// every legacy site repeated by hand. For proof-of-possession signatures made
// by a per-device key (not a trust anchor). Callers that must separate
// purposes include the purpose in msg themselves.
func VerifyDetached(pub ed25519.PublicKey, msg, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public key has %d bytes", ErrMalformed, len(pub))
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature has %d bytes, want %d", ErrMalformed, len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// Scheme is a deployed, bare-signature message construction. It has NO domain
// separation on the wire; the Verifier confines it to the keys allowed for the
// purpose the caller names.
type Scheme struct {
	Name    string
	Message func(input []byte) []byte
}

// Predefined schemes: they reproduce, byte for byte, what is deployed today.
var (
	// SchemeMessage: the signature covers input as is. Instance/trial/enter
	// tokens (claims bytes) and raw 32-byte digests passed in as input
	// (addon bundle counter-signature, signed releases) use it.
	SchemeMessage = Scheme{Name: "message", Message: func(in []byte) []byte { return in }}
	// SchemeSHA256Raw: the signature covers the 32 raw bytes of sha256(input)
	// (kernel security.VerifyBundle, hub signer, order counter-signature).
	SchemeSHA256Raw = Scheme{Name: "sha256-raw", Message: func(in []byte) []byte {
		s := sha256.Sum256(in)
		return s[:]
	}}
	// SchemeSHA256Hex: the signature covers the lowercase hex STRING of
	// sha256(input) (ops asteby-bundle and asteby-update).
	SchemeSHA256Hex = Scheme{Name: "sha256-hex", Message: func(in []byte) []byte {
		s := sha256.Sum256(in)
		return []byte(hex.EncodeToString(s[:]))
	}}
)

// SchemePrefixed: the signature covers prefix||input (hub orders:
// "asteby-order-v1\x00"; activation bootstrap: "asteby/act-bootstrap/v1\n").
func SchemePrefixed(prefix string) Scheme {
	return Scheme{Name: "prefixed", Message: func(in []byte) []byte {
		return append([]byte(prefix), in...)
	}}
}

// SchemePrefixedSHA256: the signature covers sha256(prefix||input) (brand
// packs).
func SchemePrefixedSHA256(prefix string) Scheme {
	return Scheme{Name: "prefixed-sha256", Message: func(in []byte) []byte {
		h := sha256.New()
		h.Write([]byte(prefix))
		h.Write(in)
		return h.Sum(nil)
	}}
}

// VerifyLegacy checks sig (raw bytes, 64 long) over scheme.Message(input)
// against every key active at now and allowed for purpose; it succeeds if any
// one verifies (rotation). Cross-purpose protection for these formats is key
// scoping only: a key that is not declared for purpose is never tried.
func (v *Verifier) VerifyLegacy(purpose Purpose, s Scheme, input, sig []byte) (*Result, error) {
	if v == nil || v.Keys() == nil {
		return nil, fmt.Errorf("%w: no key set", ErrInvalidConfig)
	}
	if !purpose.valid() || s.Message == nil {
		return nil, fmt.Errorf("%w: invalid purpose or scheme", ErrInvalidConfig)
	}
	if v.policy.RequireNative {
		return nil, ErrDowngrade
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature has %d bytes, want %d", ErrMalformed, len(sig), ed25519.SignatureSize)
	}
	cands, err := v.Keys().candidates(purpose, v.clock(), v.policy.Leeway)
	if err != nil {
		return nil, err
	}
	msg := s.Message(input)
	for _, k := range cands {
		if ed25519.Verify(k.Public, msg, sig) {
			return &Result{KeyID: k.ID, Purpose: purpose, Legacy: true, Scheme: s.Name}, nil
		}
	}
	return nil, ErrBadSignature
}
