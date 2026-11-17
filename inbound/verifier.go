package inbound

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// VerifyRequest is everything a Verifier may look at. Body is the exact raw
// bytes received (already size-limited).
type VerifyRequest struct {
	Org    uuid.UUID
	Method string
	// Path is the request path as received (r.URL.Path); it is part of the
	// signed string, so the host must not rewrite it before the handler.
	Path   string
	Header http.Header
	Body   []byte
}

// Verifier authenticates one inbound request for one organization. It is the
// minimal contract the webhooks package profiles (hmac_simple,
// hmac_canonical_v1, bearer, bearer+hmac_canonical_v1) satisfy. Any non-nil
// error is answered as a uniform 401; the reason is never sent to the caller.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) error
}

// VerifierFunc adapts a function to Verifier.
type VerifierFunc func(ctx context.Context, req VerifyRequest) error

func (f VerifierFunc) Verify(ctx context.Context, req VerifyRequest) error { return f(ctx, req) }

// SecretResolver returns the active secrets of an organization (several while
// rotating). Return an empty slice or an error for an unknown organization.
type SecretResolver func(ctx context.Context, org uuid.UUID) ([][]byte, error)

const (
	// ProfileHMACCanonicalV1 is the registered name of the profile below.
	ProfileHMACCanonicalV1 = "hmac_canonical_v1"

	// DefaultSkew is the accepted clock difference (+-300 s).
	DefaultSkew = 300 * time.Second
	// DefaultNonceTTL is how long a nonce is remembered (10 min, > 2*skew).
	DefaultNonceTTL = 10 * time.Minute

	maxNonceLen = 128
)

// HMACCanonicalV1 verifies
//
//	HMAC-SHA256(secret, "{ts}.{nonce}.{METHOD}.{path}.{sha256hex(body)}")
//
// with ts (unix seconds) and nonce in headers. It is a local, self-contained
// implementation so this package is testable on its own; hosts normally plug
// the webhooks package profile instead. Nonces are scoped per organization and
// recorded only after the signature is valid.
type HMACCanonicalV1 struct {
	Secrets SecretResolver
	// Header names; defaults X-Metacore-Timestamp / -Nonce / -Signature.
	TimestampHeader, NonceHeader, SignatureHeader string
	Skew                                          time.Duration
	NonceTTL                                      time.Duration
	// MaxNonces caps remembered nonces (default 100000). When full of live
	// entries the verifier fails closed.
	MaxNonces int
	Now       func() time.Time

	mu     sync.Mutex
	nonces map[string]time.Time
}

// SignCanonicalV1 computes the canonical signature (helper for clients and tests).
func SignCanonicalV1(secret []byte, ts, nonce, method, path string, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "." + nonce + "." + strings.ToUpper(method) + "." + path + "." + hex.EncodeToString(sum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

var (
	errBadSignature = errors.New("signature rejected")
	errReplay       = errors.New("nonce replay")
)

func (v *HMACCanonicalV1) hdr(set, def string) string {
	if set != "" {
		return set
	}
	return def
}

// Verify implements Verifier.
func (v *HMACCanonicalV1) Verify(ctx context.Context, req VerifyRequest) error {
	if v.Secrets == nil {
		return errors.New("no secret resolver")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	skew := v.Skew
	if skew <= 0 {
		skew = DefaultSkew
	}
	tsStr := req.Header.Get(v.hdr(v.TimestampHeader, "X-Metacore-Timestamp"))
	nonce := req.Header.Get(v.hdr(v.NonceHeader, "X-Metacore-Nonce"))
	sig := strings.TrimPrefix(strings.TrimSpace(req.Header.Get(v.hdr(v.SignatureHeader, "X-Metacore-Signature"))), "sha256=")
	if tsStr == "" || nonce == "" || sig == "" || len(nonce) > maxNonceLen {
		return errBadSignature
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errBadSignature
	}
	d := now().Sub(time.Unix(ts, 0))
	if d < 0 {
		d = -d
	}
	if d > skew {
		return errBadSignature
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return errBadSignature
	}
	secrets, err := v.Secrets(ctx, req.Org)
	if err != nil || len(secrets) == 0 {
		return errBadSignature
	}
	ok := false
	for _, s := range secrets {
		want, _ := hex.DecodeString(SignCanonicalV1(s, tsStr, nonce, req.Method, req.Path, req.Body))
		if hmac.Equal(want, got) {
			ok = true
		}
	}
	if !ok {
		return errBadSignature
	}
	return v.record(req.Org.String()+"|"+nonce, now())
}

func (v *HMACCanonicalV1) record(key string, now time.Time) error {
	ttl := v.NonceTTL
	if ttl <= 0 {
		ttl = DefaultNonceTTL
	}
	max := v.MaxNonces
	if max <= 0 {
		max = 100000
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.nonces == nil {
		v.nonces = map[string]time.Time{}
	}
	if exp, ok := v.nonces[key]; ok && now.Before(exp) {
		return errReplay
	}
	if len(v.nonces) >= max {
		for k, exp := range v.nonces {
			if !now.Before(exp) {
				delete(v.nonces, k)
			}
		}
		if len(v.nonces) >= max {
			return errors.New("nonce cache full")
		}
	}
	v.nonces[key] = now.Add(ttl)
	return nil
}
