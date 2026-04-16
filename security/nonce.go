package security

import (
	"sync"
	"time"
)

// NonceCache is a small TTL-bounded set that rejects repeated HMAC nonces.
// The HMAC signature alone protects forgery; it does NOT protect replay —
// within the 5-minute maxSkew window a captured request could be replayed
// verbatim. Addons that care (payments, state-changing ops) should require
// X-Metacore-Nonce and plug a NonceCache into their verify path.
//
// Implementation: one sync.Map keyed by nonce with a background sweeper
// that drops entries older than ttl. Good for 10k nonces/min; swap for
// Redis when you outgrow one process.
type NonceCache struct {
	ttl   time.Duration
	seen  sync.Map // nonce(string) → expiresAt (time.Time)
	clock func() time.Time
}

// NewNonceCache returns a cache that remembers nonces for at least ttl
// (twice the HMAC skew window is a good default).
func NewNonceCache(ttl time.Duration) *NonceCache {
	n := &NonceCache{ttl: ttl, clock: time.Now}
	go n.sweep()
	return n
}

// CheckAndRecord returns an error if the nonce was seen within the TTL,
// otherwise records it. Empty nonces are rejected — callers that don't send
// one should not consult this cache.
func (n *NonceCache) CheckAndRecord(nonce string) error {
	if nonce == "" {
		return errNonceRequired
	}
	now := n.clock()
	if prev, loaded := n.seen.LoadOrStore(nonce, now.Add(n.ttl)); loaded {
		if exp, ok := prev.(time.Time); ok && now.Before(exp) {
			return errNonceReplay
		}
		// Expired — overwrite.
		n.seen.Store(nonce, now.Add(n.ttl))
	}
	return nil
}

func (n *NonceCache) sweep() {
	t := time.NewTicker(n.ttl)
	defer t.Stop()
	for now := range t.C {
		n.seen.Range(func(k, v any) bool {
			if exp, ok := v.(time.Time); ok && now.After(exp) {
				n.seen.Delete(k)
			}
			return true
		})
	}
}

type replayErr string

func (e replayErr) Error() string { return string(e) }

const (
	errNonceReplay   = replayErr("replay: nonce already used")
	errNonceRequired = replayErr("replay: missing nonce")
)
