package signedenvelope

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// UnixWindow reads a window from a JSON object whose nbfField/expField hold
// unix seconds (either may be empty or absent = unbounded). It is what the
// instance-licence claims (iat/exp) and most tokens need.
func UnixWindow(nbfField, expField string) WindowFunc {
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
			if !ok {
				return nil, nil
			}
			var sec int64
			if err := json.Unmarshal(raw, &sec); err != nil {
				return nil, fmt.Errorf("%w: %q is not unix seconds", ErrMalformed, f)
			}
			t := time.Unix(sec, 0).UTC()
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

// Policy tunes a Verifier.
type Policy struct {
	// RequireNative refuses every legacy format (downgrade protection). It is
	// the end state of a migration; flip it per purpose with a dedicated
	// Verifier once the issuer emits "asteby1." tokens.
	RequireNative bool
	// Window, if set, is applied to the payload of Open results. Absent window
	// fields mean "no bound". Leeway widens both bounds (clock skew).
	Window WindowFunc
	Leeway time.Duration
}

// Verifier verifies material against a KeySet.
type Verifier struct {
	Keys   *KeySet
	Policy Policy
	// Now is the clock (default time.Now). Inject in tests.
	Now func() time.Time
}

// Result describes a successful verification.
type Result struct {
	KeyID   string // kid for native tokens; sigv2.LegacyKID for the legacy token
	Purpose Purpose
	Payload []byte // the verified payload (Open); nil for VerifyLegacy
	Legacy  bool   // true when a pre-native format was accepted
	Scheme  string // legacy scheme name, "" for native
	Window  Window
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now().UTC()
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
// sig>" token (verified against any active key allowed for want:
// the legacy token carries no kid, so every active key declared for want is a
// candidate).
func (v *Verifier) Open(want Purpose, token string) (*Result, error) {
	if v == nil || v.Keys == nil {
		return nil, fmt.Errorf("%w: no key set", ErrInvalidConfig)
	}
	if !want.valid() {
		return nil, fmt.Errorf("%w: invalid purpose", ErrInvalidConfig)
	}
	now := v.now()
	var res *Result
	if sigv2.IsEnvelope(token) {
		op, err := sigv2.Open(token, string(want), v.Keys, sigv2.Policy{RequireV2: true}, now)
		if err != nil {
			return nil, mapV2(err)
		}
		res = &Result{KeyID: op.KID, Purpose: want, Payload: op.Payload}
	} else {
		if v.Policy.RequireNative {
			return nil, ErrDowngrade
		}
		cands, err := v.Keys.candidates(want, now)
		if err != nil {
			return nil, err
		}
		// One sigv2 call per candidate through a one-key ring: sigv2 owns the
		// legacy token parsing and the signature check.
		var last error = ErrBadSignature
		for _, k := range cands {
			ring := sigv2.NewStaticKeyring().Add(sigv2.LegacyKID, k.Public)
			op, err := sigv2.Open(token, string(want), ring, sigv2.Policy{}, now)
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
	if v.Policy.Window != nil {
		w, err := v.Policy.Window(res.Payload)
		if err != nil {
			return nil, err
		}
		if w.NotBefore != nil && now.Add(v.Policy.Leeway).Before(*w.NotBefore) {
			return nil, ErrNotYetValid
		}
		if w.Expires != nil && !now.Add(-v.Policy.Leeway).Before(*w.Expires) {
			return nil, ErrExpired
		}
		res.Window = w
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
	if v == nil || v.Keys == nil {
		return nil, fmt.Errorf("%w: no key set", ErrInvalidConfig)
	}
	if !purpose.valid() || s.Message == nil {
		return nil, fmt.Errorf("%w: invalid purpose or scheme", ErrInvalidConfig)
	}
	if v.Policy.RequireNative {
		return nil, ErrDowngrade
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature has %d bytes, want %d", ErrMalformed, len(sig), ed25519.SignatureSize)
	}
	cands, err := v.Keys.candidates(purpose, v.now())
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
