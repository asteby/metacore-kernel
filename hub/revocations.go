package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Revocation list contract (C1):
//
//	GET {hub}/v1/marketplace/revocations →
//	  {"generated_at":RFC3339,
//	   "revoked":[{"addon_key","version"("*" = all),"reason","revoked_at"}],
//	   "signature":"<base64 Ed25519 over the canonical JSON of {generated_at,revoked}>"}
//
// The signature is verified against the central marketplace key(s) supplied
// by the caller (installer.Installer.PublicKeys: the same trust anchor that
// verifies bundles, see installer/central_trust.go). This package keeps zero
// kernel-internal dependencies, so the keys arrive through a func.

const (
	revocationsPath    = "/v1/marketplace/revocations"
	maxRevocationsBody = 4 << 20
	// DefaultRevocationTTL is how long a verified list is served from cache
	// before the hub is asked again (conditional GET with If-None-Match).
	DefaultRevocationTTL = 5 * time.Minute
)

// ErrRevocationsUnavailable means no verified revocation list is known: the
// hub failed (or served an invalid/unsigned list) and nothing valid was cached.
var ErrRevocationsUnavailable = errors.New("hub: revocation list unavailable")

// ErrRevocationSignature means the served list failed signature verification.
var ErrRevocationSignature = errors.New("hub: revocation list signature invalid")

// Revocation is one revoked addon version.
type Revocation struct {
	AddonKey  string `json:"addon_key"`
	Version   string `json:"version"` // "*" revokes every version
	Reason    string `json:"reason"`
	RevokedAt string `json:"revoked_at"`
}

// RevocationList is the verified payload.
type RevocationList struct {
	GeneratedAt string       `json:"generated_at"`
	Revoked     []Revocation `json:"revoked"`
}

// Lookup returns the revocation covering addonKey@version, if any.
func (l *RevocationList) Lookup(addonKey, version string) (Revocation, bool) {
	if l == nil {
		return Revocation{}, false
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	for _, r := range l.Revoked {
		if r.AddonKey != addonKey {
			continue
		}
		rv := strings.TrimPrefix(strings.TrimSpace(r.Version), "v")
		if rv == "*" || rv == version {
			return r, true
		}
	}
	return Revocation{}, false
}

type revocationWire struct {
	GeneratedAt string       `json:"generated_at"`
	Revoked     []Revocation `json:"revoked"`
	Signature   string       `json:"signature"`
}

// signedCandidates returns the byte strings the hub may have signed. The
// contract says "canonical JSON of {generated_at,revoked}" without fixing key
// order/escaping, so we accept the contract field order (struct order) and the
// alphabetically-sorted-keys form, each with and without HTML escaping. All
// four encode exactly the same data, so accepting any of them cannot admit a
// forged list.
func signedCandidates(l RevocationList) [][]byte {
	if l.Revoked == nil {
		l.Revoked = []Revocation{}
	}
	sorted := map[string]any{"generated_at": l.GeneratedAt}
	items := make([]any, 0, len(l.Revoked))
	for _, r := range l.Revoked {
		items = append(items, map[string]any{
			"addon_key": r.AddonKey, "version": r.Version, "reason": r.Reason, "revoked_at": r.RevokedAt,
		})
	}
	sorted["revoked"] = items
	var out [][]byte
	for _, v := range []any{l, sorted} {
		for _, esc := range []bool{true, false} {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(esc)
			if enc.Encode(v) == nil {
				out = append(out, bytes.TrimRight(buf.Bytes(), "\n"))
			}
		}
	}
	return out
}

// VerifyRevocationList parses a raw response body and verifies its signature
// against any of keys. It fails closed: no keys, empty/invalid signature or
// no match all return an error.
func VerifyRevocationList(raw []byte, keys []ed25519.PublicKey) (*RevocationList, error) {
	var w revocationWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("hub: decode revocations: %w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no trusted marketplace key", ErrRevocationSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(w.Signature))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: malformed signature", ErrRevocationSignature)
	}
	list := RevocationList{GeneratedAt: w.GeneratedAt, Revoked: w.Revoked}
	for _, msg := range signedCandidates(list) {
		for _, k := range keys {
			if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, msg, sig) {
				if list.Revoked == nil {
					list.Revoked = []Revocation{}
				}
				return &list, nil
			}
		}
	}
	return nil, ErrRevocationSignature
}

// RevocationChecker fetches, verifies and caches the revocation list. Safe for
// concurrent use.
//
// Fail-safe policy: a hub outage, a non-2xx, a bad signature or an older
// (replayed) list never discards the last valid list, so a known revocation is
// never forgotten ("never fail open on a known revocation").
type RevocationChecker struct {
	client    *Client
	keys      func() []ed25519.PublicKey
	ttl       time.Duration
	cachePath string
	now       func() time.Time

	mu        sync.Mutex
	list      *RevocationList
	raw       []byte
	etag      string
	fetchedAt time.Time
}

// RevocationOption configures a RevocationChecker.
type RevocationOption func(*RevocationChecker)

// WithRevocationTTL sets the cache TTL (default 5 minutes).
func WithRevocationTTL(d time.Duration) RevocationOption {
	return func(r *RevocationChecker) {
		if d > 0 {
			r.ttl = d
		}
	}
}

// WithRevocationCacheFile persists the last verified raw response so a
// restart while the hub is down still knows the revocations. The file is
// re-verified against the trusted keys on load; it is never trusted blindly.
func WithRevocationCacheFile(path string) RevocationOption {
	return func(r *RevocationChecker) { r.cachePath = path }
}

// NewRevocationChecker builds a checker. keys supplies the trusted central
// marketplace public keys at verification time (so key rotation via
// Installer.AppendTrustedPubKey is picked up).
func (c *Client) NewRevocationChecker(keys func() []ed25519.PublicKey, opts ...RevocationOption) *RevocationChecker {
	r := &RevocationChecker{client: c, keys: keys, ttl: DefaultRevocationTTL, now: time.Now}
	for _, o := range opts {
		o(r)
	}
	return r
}

// List returns the current verified list, refreshing it when the TTL elapsed.
// If the refresh fails, the last valid list is returned with a nil error (the
// failure is logged); an error is returned only when no valid list is known.
func (r *RevocationChecker) List(ctx context.Context) (*RevocationList, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.list == nil {
		r.loadFromDiskLocked()
	}
	if r.list != nil && r.now().Sub(r.fetchedAt) < r.ttl {
		return r.list, nil
	}
	if err := r.refreshLocked(ctx); err != nil {
		if r.list != nil {
			slog.Warn("hub.revocations_refresh_failed_using_cache", "err", err.Error(), "generated_at", r.list.GeneratedAt)
			// Back off for a short window so a down hub is not hammered on
			// every install; the cached list keeps being enforced.
			r.fetchedAt = r.now().Add(-r.ttl + minDuration(r.ttl, 30*time.Second))
			return r.list, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrRevocationsUnavailable, err)
	}
	return r.list, nil
}

// Refresh forces a conditional fetch ignoring the TTL and reports the error
// (the cached list, if any, is kept).
func (r *RevocationChecker) Refresh(ctx context.Context) (*RevocationList, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.list == nil {
		r.loadFromDiskLocked()
	}
	if err := r.refreshLocked(ctx); err != nil {
		return r.list, err
	}
	return r.list, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (r *RevocationChecker) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.client.buildURL(revocationsPath), nil)
	if err != nil {
		return err
	}
	r.client.authorise(req)
	req.Header.Set("Accept", "application/json")
	if r.etag != "" && r.list != nil {
		req.Header.Set("If-None-Match", r.etag)
	}
	resp, err := r.client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("hub: GET revocations: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && r.list != nil {
		r.fetchedAt = r.now()
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return statusErr(resp, "revocations")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRevocationsBody+1))
	if err != nil {
		return fmt.Errorf("hub: read revocations: %w", err)
	}
	if len(raw) > maxRevocationsBody {
		return fmt.Errorf("hub: revocations body exceeds %d bytes", maxRevocationsBody)
	}
	list, err := VerifyRevocationList(raw, r.keys())
	if err != nil {
		return err
	}
	if r.list != nil && olderThan(list.GeneratedAt, r.list.GeneratedAt) {
		return fmt.Errorf("hub: revocations list older than cached (%s < %s): possible replay", list.GeneratedAt, r.list.GeneratedAt)
	}
	r.list, r.raw, r.etag, r.fetchedAt = list, raw, resp.Header.Get("ETag"), r.now()
	r.persistLocked()
	return nil
}

// olderThan reports whether a is strictly before b (both RFC3339). Unparseable
// timestamps are never treated as older, so a format quirk can't pin a list.
func olderThan(a, b string) bool {
	ta, err1 := time.Parse(time.RFC3339, a)
	tb, err2 := time.Parse(time.RFC3339, b)
	return err1 == nil && err2 == nil && ta.Before(tb)
}

func (r *RevocationChecker) persistLocked() {
	if r.cachePath == "" {
		return
	}
	if err := os.WriteFile(r.cachePath, r.raw, 0o600); err != nil {
		slog.Warn("hub.revocations_cache_write_failed", "err", err.Error())
	}
}

func (r *RevocationChecker) loadFromDiskLocked() {
	if r.cachePath == "" {
		return
	}
	raw, err := os.ReadFile(r.cachePath)
	if err != nil {
		return
	}
	list, err := VerifyRevocationList(raw, r.keys())
	if err != nil {
		slog.Warn("hub.revocations_cache_file_rejected", "err", err.Error())
		return
	}
	// fetchedAt stays zero so the first List() tries the hub; the disk copy
	// is only the fallback. No ETag: we have not confirmed it is current.
	r.list, r.raw = list, raw
}
