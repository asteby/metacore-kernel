package permission

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// cacheEntry stores the resolved capability set for one user plus its
// expiration. The slice is treated as immutable once stored — callers that
// need to mutate the list must copy first.
type cacheEntry struct {
	caps      []Capability
	expiresAt time.Time
}

// capCache is a tiny per-user TTL cache used by Service to avoid hammering
// the store on every request. It is safe for concurrent use.
//
// Scope: the cache is keyed purely by user id. Organization scoping is the
// store's job; if an app needs an (org,user) cache it can compose its own on
// top of PermissionStore.
type capCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	entries map[uuid.UUID]cacheEntry
}

func newCapCache(ttl time.Duration) *capCache {
	return &capCache{
		ttl:     ttl,
		entries: make(map[uuid.UUID]cacheEntry),
	}
}

// get returns the cached capability list (ok=true) or ok=false on miss/expiry.
// On expiry the entry is lazily swept by subsequent puts; get itself stays
// read-only so hot paths do not upgrade to a write lock.
func (c *capCache) get(userID uuid.UUID) ([]Capability, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[userID]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.caps, true
}

// put stores caps for userID with the configured TTL. A nil receiver, a
// zero/negative TTL, or uuid.Nil are no-ops — callers can invoke put
// unconditionally.
func (c *capCache) put(userID uuid.UUID, caps []Capability) {
	if c == nil || c.ttl <= 0 || userID == uuid.Nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[userID] = cacheEntry{
		caps:      caps,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// invalidate drops any entry for userID. Safe on nil receiver.
func (c *capCache) invalidate(userID uuid.UUID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, userID)
}

// invalidateAll drops every entry. Used when role->capability mappings
// change, since those affect many users at once.
func (c *capCache) invalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[uuid.UUID]cacheEntry)
}
