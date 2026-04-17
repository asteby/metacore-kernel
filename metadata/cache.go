package metadata

import (
	"sync"
	"time"
)

// cache is a tiny in-memory TTL cache shared by Service. It is intentionally
// private to the package — apps that want a different cache (Redis, LRU,
// whatever) should wire their own Service methods via transformers rather
// than swap this out.
//
// The zero TTL disables caching entirely: Set stores nothing and Get always
// misses. This is the documented behaviour of Config{CacheTTL: 0}.
type cache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	ttl     time.Duration
	now     func() time.Time // injectable for tests
}

type cacheEntry struct {
	value     interface{}
	expiresAt time.Time
}

// newCache constructs a cache with the given TTL. A non-positive ttl disables
// the cache.
func newCache(ttl time.Duration) *cache {
	return &cache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Get returns the cached value for key if present and not expired.
func (c *cache) Get(key string) (interface{}, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.After(c.now()) {
		// Expired — drop it so subsequent reads don't pay the check.
		c.mu.Lock()
		if cur, still := c.entries[key]; still && cur.expiresAt.Equal(entry.expiresAt) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return nil, false
	}
	return entry.value, true
}

// Set stores value under key with the cache's configured TTL. If TTL is
// non-positive the call is a no-op.
func (c *cache) Set(key string, value interface{}) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.entries[key] = cacheEntry{
		value:     value,
		expiresAt: c.now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// Invalidate removes a single key from the cache.
func (c *cache) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// InvalidateAll drops every entry. Called by Service.InvalidateCache.
func (c *cache) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]cacheEntry)
	c.mu.Unlock()
}

// len returns the number of live entries. Exported to tests only via the
// package boundary; production code has no business asking.
func (c *cache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
