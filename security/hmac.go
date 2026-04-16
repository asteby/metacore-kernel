// Package security provides HMAC signing for outbound webhooks to remote
// addons and scoped capability enforcement for addon DB/HTTP access.
package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// Signer produces signatures the addon verifies to trust a call is from the host.
type Signer struct {
	// Secret is the per-installation secret negotiated at install time.
	Secret []byte
	// Clock is overridable for testing; defaults to time.Now.
	Clock func() time.Time
}

// NewSigner builds a signer from a per-installation secret.
func NewSigner(secret []byte) *Signer {
	return &Signer{Secret: secret, Clock: time.Now}
}

// Sign returns the signature headers to attach to an outbound webhook call
// without a replay nonce. Prefer SignWithNonce — plain Sign kept for legacy
// callers that have not yet wired a NonceCache on the receiver side.
func (s *Signer) Sign(method, path string, body []byte) map[string]string {
	return s.SignWithNonce(method, path, body, "")
}

// SignWithNonce is Sign plus a per-request nonce bound into the HMAC payload
// so a captured request cannot be replayed verbatim within the timestamp
// window. The nonce is also emitted as X-Metacore-Nonce; the receiver must
// reject duplicates with a NonceCache.
//
// Headers:
//
//	X-Metacore-Timestamp: <unix seconds>
//	X-Metacore-Nonce:     <opaque, unique per call>  (only when nonce != "")
//	X-Metacore-Signature: sha256=<hex>
//
// Signed string: "<ts>.<method>.<path>.<nonce>.<sha256(body)>" when nonce
// is non-empty, else "<ts>.<method>.<path>.<sha256(body)>" (legacy form).
func (s *Signer) SignWithNonce(method, path string, body []byte, nonce string) map[string]string {
	ts := strconv.FormatInt(s.Clock().Unix(), 10)
	bodyHash := sha256.Sum256(body)
	var payload string
	if nonce == "" {
		payload = fmt.Sprintf("%s.%s.%s.%s", ts, method, path, hex.EncodeToString(bodyHash[:]))
	} else {
		payload = fmt.Sprintf("%s.%s.%s.%s.%s", ts, method, path, nonce, hex.EncodeToString(bodyHash[:]))
	}
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write([]byte(payload))
	h := map[string]string{
		"X-Metacore-Timestamp": ts,
		"X-Metacore-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	}
	if nonce != "" {
		h["X-Metacore-Nonce"] = nonce
	}
	return h
}

// Verify checks a signature without a replay nonce. Receivers that care about
// replay protection (state-changing webhooks) should use VerifyWithNonce and
// plug a NonceCache.
func (s *Signer) Verify(method, path string, body []byte, timestamp, signature string, maxSkew time.Duration) error {
	return s.VerifyWithNonce(method, path, body, timestamp, "", signature, maxSkew)
}

// VerifyWithNonce is Verify plus nonce binding. It does NOT record the nonce
// — caller feeds it through a NonceCache so the replay check is enforced
// across calls. Leaving nonce empty reproduces the legacy payload format.
func (s *Signer) VerifyWithNonce(method, path string, body []byte, timestamp, nonce, signature string, maxSkew time.Duration) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("bad timestamp")
	}
	skew := s.Clock().Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > maxSkew {
		return fmt.Errorf("timestamp outside window")
	}
	bodyHash := sha256.Sum256(body)
	var payload string
	if nonce == "" {
		payload = fmt.Sprintf("%s.%s.%s.%s", timestamp, method, path, hex.EncodeToString(bodyHash[:]))
	} else {
		payload = fmt.Sprintf("%s.%s.%s.%s.%s", timestamp, method, path, nonce, hex.EncodeToString(bodyHash[:]))
	}
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write([]byte(payload))
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}
