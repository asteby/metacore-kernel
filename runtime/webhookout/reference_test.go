package webhookout

// Reference verifier: an INDEPENDENT transcription of the receiving side's
// algorithm (the control-plane `coreCanonical` + `coreAuthenticate` signature
// checks), kept free of any production code of this package so the round trip
// proves the client emits what the verifier expects. If the receiver's scheme
// changes, this file and docs/spec/v3/webhook-signing.md change together.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var refIdemKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func refCanonical(method, path, rawQuery, idemKey, actor, ts string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		"asteby-req-v1", strings.ToUpper(method), path, rawQuery, idemKey, actor, ts, hex.EncodeToString(sum[:]),
	}, "\n"))
}

// refVerify checks a request the way the receiver does for a write, and
// returns the public key (hex) that signed it.
func refVerify(r *http.Request, body []byte, now time.Time) (string, error) {
	if r.Header.Get("X-Signature-Scheme") != "asteby-req-v1" {
		return "", errors.New("invalid_signature_headers: scheme")
	}
	sigHdr := r.Header.Get("X-Signature")
	tsHdr := r.Header.Get("X-Signature-Timestamp")
	pubHdr := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Instance-Pubkey")))
	if sigHdr == "" || tsHdr == "" || pubHdr == "" {
		return "", errors.New("invalid_signature_headers: missing")
	}
	if !strings.HasPrefix(sigHdr, "ed25519:") {
		return "", errors.New("invalid_signature_headers: label")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sigHdr, "ed25519:"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", errors.New("invalid_signature_headers: sig")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(tsHdr), 10, 64)
	if err != nil {
		return "", errors.New("invalid_signature_headers: ts")
	}
	if d := now.Sub(time.Unix(n, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return "", errors.New("stale_timestamp")
	}
	pub, err := hex.DecodeString(pubHdr)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", errors.New("invalid_signature_headers: pubkey")
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	actor := strings.TrimSpace(r.Header.Get("X-Actor"))
	if !refIdemKey.MatchString(idem) {
		return "", errors.New("idempotency_key_required")
	}
	if actor == "" || !strings.Contains(actor, "@") {
		return "", errors.New("actor_required")
	}
	msg := refCanonical(r.Method, r.URL.Path, r.URL.RawQuery, idem, actor, tsHdr, body)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return "", errors.New("signature_mismatch")
	}
	return pubHdr, nil
}
