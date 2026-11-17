package signing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// VerifyRequest is everything a receiver hands to Verify. Its field list,
// names, types and ORDER are identical to inbound.VerifyRequest, so the
// receiver converts with a plain type conversion:
//
//	signing.VerifyRequest(req)
//
// Path is the path as received (signing.PathOf(r.URL)); it is part of the
// signed string, so the host must not rewrite it before verifying. Body is the
// exact raw bytes received.
type VerifyRequest struct {
	Org    uuid.UUID
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// SecretsFunc resolves the secrets of an organization. Return SEVERAL entries
// while a secret is being rotated (overlap window): any of them verifies. An
// empty slice or an error rejects the request (unknown organization).
type SecretsFunc func(ctx context.Context, org uuid.UUID) ([]Secrets, error)

// Verifier authenticates inbound requests for a Spec. It is the object the
// receiver (package inbound) plugs in; it satisfies
//
//	interface{ Verify(ctx context.Context, req VerifyRequest) error }
//
// (convert the request type, see VerifyRequest). Any non-nil error means
// "reject"; use CodeOf for the stable reason, and answer a uniform 401 to the
// caller. Nonces are remembered per organization and recorded only after the
// signature (and bearer) are valid.
type Verifier struct {
	Spec    Spec
	Secrets SecretsFunc
	// MaxSkew: clock window (default 300 s).
	MaxSkew time.Duration
	// NonceTTL: how long a nonce is remembered (default 10 min). The verifier
	// keeps its own in-memory set; set Nonces to share one across processes.
	NonceTTL time.Duration
	// Nonces, if set, replaces the in-memory set (key = "<org>|<nonce>").
	Nonces NonceRecorder
	// MaxNonces caps the in-memory set (default 100000); full = fail closed.
	MaxNonces int
	// Now is the clock (default time.Now).
	Now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// Verify implements the receiver-side contract.
func (v *Verifier) Verify(ctx context.Context, req VerifyRequest) error {
	if v.Secrets == nil {
		return fmt.Errorf("%w: no secrets resolver", ErrMissingSecret)
	}
	cands, err := v.Secrets(ctx, req.Org)
	if err != nil || len(cands) == 0 {
		return ErrUnauthenticated
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	var last error = ErrBadSignature
	for _, sec := range cands {
		err := Verify(v.Spec, sec, Request{Method: req.Method, Path: req.Path, Body: req.Body}, req.Header,
			VerifyOptions{Now: now(), MaxSkew: v.MaxSkew})
		if err == nil {
			return v.recordNonce(req)
		}
		last = err
		var e *Error
		if !errors.As(err, &e) { // a malformed spec / missing secret will not improve with another secret
			return err
		}
	}
	return last
}

func (v *Verifier) recordNonce(req VerifyRequest) error {
	if !v.Spec.canonical() {
		return nil // hmac_simple / bearer carry no nonce
	}
	key := req.Org.String() + "|" + req.Header.Get(v.Spec.nonceHeader())
	if v.Nonces != nil {
		if err := v.Nonces.CheckAndRecord(key); err != nil {
			return ErrNonceReused
		}
		return nil
	}
	ttl := v.NonceTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	max := v.MaxNonces
	if max <= 0 {
		max = 100000
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = map[string]time.Time{}
	}
	if exp, ok := v.seen[key]; ok && now.Before(exp) {
		return ErrNonceReused
	}
	if len(v.seen) >= max {
		for k, exp := range v.seen {
			if !now.Before(exp) {
				delete(v.seen, k)
			}
		}
		if len(v.seen) >= max {
			return &Error{code: "unavailable", msg: "nonce cache full"}
		}
	}
	v.seen[key] = now.Add(ttl)
	return nil
}

// Fingerprint is the hex SHA-256 of a request body: what the sender puts in
// X-Metacore-Body-SHA256 and a receiver stores next to the idempotency key to
// answer "replayed" (same key, same fingerprint) or 409 idempotency_conflict
// (same key, different fingerprint).
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
