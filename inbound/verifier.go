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
	// MaxNonces caps remembered nonces PER ORGANIZATION (default 20000);
	// MaxOrgs caps tracked organizations (default 10000). Full -> fail closed
	// for that organization only.
	MaxNonces int
	MaxOrgs   int
	Now       func() time.Time

	mu   sync.Mutex
	orgs map[uuid.UUID]*orgNonces
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

// PreparedVerifier is an optional Verifier extension. Prepare authenticates
// the request WITHOUT consuming anything with side effects; the returned
// commit (called by the handler only after the cheap checks, rate limit and
// schema validation passed) records the replay nonce. A request rejected with
// 415/422/429 therefore never burns a nonce, and an unauthenticated or
// rejected request can never fill the nonce cache.
type PreparedVerifier interface {
	Verifier
	Prepare(ctx context.Context, req VerifyRequest) (commit func() error, err error)
}

// Verify implements Verifier (authenticate and record the nonce at once).
func (v *HMACCanonicalV1) Verify(ctx context.Context, req VerifyRequest) error {
	commit, err := v.Prepare(ctx, req)
	if err != nil {
		return err
	}
	return commit()
}

// Prepare implements PreparedVerifier.
func (v *HMACCanonicalV1) Prepare(ctx context.Context, req VerifyRequest) (func() error, error) {
	if v.Secrets == nil {
		return nil, errors.New("no secret resolver")
	}
	now := v.now()
	skew := v.Skew
	if skew <= 0 {
		skew = DefaultSkew
	}
	tsStr := req.Header.Get(v.hdr(v.TimestampHeader, "X-Metacore-Timestamp"))
	nonce := req.Header.Get(v.hdr(v.NonceHeader, "X-Metacore-Nonce"))
	sig := strings.TrimPrefix(strings.TrimSpace(req.Header.Get(v.hdr(v.SignatureHeader, "X-Metacore-Signature"))), "sha256=")
	if tsStr == "" || nonce == "" || sig == "" || len(nonce) > maxNonceLen {
		return nil, errBadSignature
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return nil, errBadSignature
	}
	d := now().Sub(time.Unix(ts, 0))
	if d < 0 {
		d = -d
	}
	if d > skew {
		return nil, errBadSignature
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return nil, errBadSignature
	}
	secrets, err := v.Secrets(ctx, req.Org)
	if err != nil || len(secrets) == 0 {
		return nil, errBadSignature
	}
	ok := false
	for _, s := range secrets {
		want, _ := hex.DecodeString(SignCanonicalV1(s, tsStr, nonce, req.Method, req.Path, req.Body))
		if hmac.Equal(want, got) {
			ok = true
		}
	}
	if !ok {
		return nil, errBadSignature
	}
	// Peek so an obvious replay is refused before any further work.
	if v.seen(req.Org, nonce, now()) {
		return nil, errReplay
	}
	org := req.Org
	return func() error { return v.record(org, nonce, now()) }, nil
}

func (v *HMACCanonicalV1) now() func() time.Time {
	if v.Now != nil {
		return v.Now
	}
	return time.Now
}

type nonceEnt struct {
	key string
	exp time.Time
}

// orgNonces is one organization's replay window: a set plus a FIFO of expiries
// (TTL is constant, so insertion order is expiry order and eviction is
// amortized O(1) without scanning the set).
type orgNonces struct {
	set  map[string]time.Time
	q    []nonceEnt
	head int
}

func (o *orgNonces) expire(now time.Time) {
	for o.head < len(o.q) && !now.Before(o.q[o.head].exp) {
		e := o.q[o.head]
		if cur, ok := o.set[e.key]; ok && !now.Before(cur) {
			delete(o.set, e.key)
		}
		o.head++
	}
	if o.head > 1024 && o.head*2 > len(o.q) {
		o.q = append([]nonceEnt(nil), o.q[o.head:]...)
		o.head = 0
	}
}

func (v *HMACCanonicalV1) limits() (perOrg, orgs int, ttl time.Duration) {
	perOrg, orgs, ttl = v.MaxNonces, v.MaxOrgs, v.NonceTTL
	if perOrg <= 0 {
		perOrg = 20000
	}
	if orgs <= 0 {
		orgs = 10000
	}
	if ttl <= 0 {
		ttl = DefaultNonceTTL
	}
	return
}

func (v *HMACCanonicalV1) seen(org uuid.UUID, nonce string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.orgs[org]
	if o == nil {
		return false
	}
	o.expire(now)
	exp, ok := o.set[nonce]
	return ok && now.Before(exp)
}

// record stores the nonce. The cap is PER ORGANIZATION: one tenant filling its
// own window can only lock itself out, never other organizations.
func (v *HMACCanonicalV1) record(org uuid.UUID, nonce string, now time.Time) error {
	perOrg, maxOrgs, ttl := v.limits()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.orgs == nil {
		v.orgs = map[uuid.UUID]*orgNonces{}
	}
	o := v.orgs[org]
	if o == nil {
		if len(v.orgs) >= maxOrgs {
			for k, x := range v.orgs { // drop idle orgs; bounded by maxOrgs
				x.expire(now)
				if len(x.set) == 0 {
					delete(v.orgs, k)
				}
			}
			if len(v.orgs) >= maxOrgs {
				return errors.New("nonce cache full")
			}
		}
		o = &orgNonces{set: map[string]time.Time{}}
		v.orgs[org] = o
	}
	o.expire(now)
	if exp, ok := o.set[nonce]; ok && now.Before(exp) {
		return errReplay
	}
	if len(o.set) >= perOrg {
		return errors.New("nonce cache full for organization")
	}
	exp := now.Add(ttl)
	o.set[nonce] = exp
	o.q = append(o.q, nonceEnt{nonce, exp})
	return nil
}
