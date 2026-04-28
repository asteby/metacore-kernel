// Package idempotency provides server-side replay caching for non-idempotent
// HTTP endpoints. Clients send an `Idempotency-Key` header (Stripe-style)
// and the kernel's middleware short-circuits duplicate requests with the
// stored response — guarantees retries from a flaky network never produce
// double-creates or double-imports.
//
// The package exposes a Store interface so apps can swap the default
// in-memory LRU for a Redis or DB-backed store when they scale to multiple
// replicas.
package idempotency

import (
	"sync"
	"time"
)

// Stored is the cached envelope of a previous successful response.
type Stored struct {
	StatusCode int
	Body       []byte
	// ContentType keeps the original Content-Type so we can replay the
	// exact headers the client expects (CSV, JSON, etc).
	ContentType string
	// ExpiresAt — entries past this point are evicted lazily on Get.
	ExpiresAt time.Time
}

// Store is the surface the middleware needs. Two methods, no transactions.
type Store interface {
	// Get returns the cached response for the key, or `nil, false` if the
	// key is unknown or has expired. Implementations MUST be safe for
	// concurrent use.
	Get(key string) (*Stored, bool)

	// Put writes the entry. TTL is computed by the caller (`ExpiresAt`)
	// so different endpoints can opt into different windows.
	Put(key string, value Stored)
}

// InMemoryStore is a goroutine-safe LRU+TTL store sized for single-replica
// apps. Entries past `ExpiresAt` are dropped lazily on Get; if the cache
// hits `maxEntries` the oldest record (by insertion order) is evicted.
type InMemoryStore struct {
	mu         sync.Mutex
	entries    map[string]*Stored
	order      []string
	maxEntries int
}

// NewInMemoryStore builds a store with a soft cap. `maxEntries <= 0` falls
// back to 10_000 — enough for typical CRUD apps; bigger workloads should
// drop in a Redis-backed Store.
func NewInMemoryStore(maxEntries int) *InMemoryStore {
	if maxEntries <= 0 {
		maxEntries = 10_000
	}
	return &InMemoryStore{
		entries:    make(map[string]*Stored, maxEntries),
		order:      make([]string, 0, maxEntries),
		maxEntries: maxEntries,
	}
}

func (s *InMemoryStore) Get(key string) (*Stored, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(v.ExpiresAt) {
		delete(s.entries, key)
		s.removeFromOrder(key)
		return nil, false
	}
	return v, true
}

func (s *InMemoryStore) Put(key string, value Stored) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[key]; !exists {
		if len(s.order) >= s.maxEntries {
			oldest := s.order[0]
			delete(s.entries, oldest)
			s.order = s.order[1:]
		}
		s.order = append(s.order, key)
	}
	s.entries[key] = &value
}

func (s *InMemoryStore) removeFromOrder(key string) {
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
