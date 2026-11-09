package webhookout

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Scheme is the wire name of the signature scheme, sent in X-Signature-Scheme.
// The manifest names it declaratively as handler.signing.scheme
// "instance-ed25519-v1" (SchemeManifest).
const (
	Scheme         = "asteby-req-v1"
	SchemeManifest = "instance-ed25519-v1"

	HeaderScheme         = "X-Signature-Scheme"
	HeaderSignature      = "X-Signature"
	HeaderTimestamp      = "X-Signature-Timestamp"
	HeaderPubKey         = "X-Instance-Pubkey"
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderActor          = "X-Actor"

	// SignatureLabel prefixes the base64 signature in X-Signature.
	SignatureLabel = "ed25519:"
	// MaxSkew is the clock drift the receiving side tolerates.
	MaxSkew = 5 * time.Minute
)

// Signer holds the instance's Ed25519 identity. The HOST injects it: the
// kernel never stores, derives or logs a private key.
type Signer interface {
	// PublicKeyHex is the 32-byte public key, hex-encoded (64 chars).
	PublicKeyHex() string
	// Sign returns the raw 64-byte Ed25519 signature of message.
	Sign(message []byte) ([]byte, error)
}

type ed25519Signer struct{ priv ed25519.PrivateKey }

// NewEd25519Signer wraps a private key the host already holds in memory.
func NewEd25519Signer(priv ed25519.PrivateKey) (Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("webhookout: invalid Ed25519 private key")
	}
	return ed25519Signer{priv: priv}, nil
}

func (s ed25519Signer) PublicKeyHex() string {
	return hex.EncodeToString(s.priv.Public().(ed25519.PublicKey))
}

func (s ed25519Signer) Sign(m []byte) ([]byte, error) { return ed25519.Sign(s.priv, m), nil }

// Canonical is the message the instance signs (asteby-req-v1):
//
//	asteby-req-v1 \n METHOD \n path \n rawQuery \n idempotencyKey \n actor \n ts \n hex(sha256(body))
//
// path is the DECODED URL path exactly as the receiver sees it (r.URL.Path,
// without any proxy prefix); rawQuery is r.URL.RawQuery; ts is the decimal
// unix-seconds string sent in X-Signature-Timestamp. The signature is bound to
// the route, so a capture cannot be replayed against another one.
func Canonical(method, path, rawQuery, idemKey, actor, ts string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		Scheme, strings.ToUpper(method), path, rawQuery, idemKey, actor, ts, hex.EncodeToString(sum[:]),
	}, "\n"))
}

// SignatureHeaders returns the values of X-Signature and X-Signature-Timestamp.
func SignatureHeaders(s Signer, method, path, rawQuery, idemKey, actor string, body []byte, now time.Time) (sig, ts string, err error) {
	ts = strconv.FormatInt(now.Unix(), 10)
	raw, err := s.Sign(Canonical(method, path, rawQuery, idemKey, actor, ts, body))
	if err != nil {
		return "", "", err
	}
	if len(raw) != ed25519.SignatureSize {
		return "", "", errors.New("webhookout: signer returned a malformed signature")
	}
	return SignatureLabel + base64.StdEncoding.EncodeToString(raw), ts, nil
}
