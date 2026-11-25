package contract

import (
	"context"
	"sync"
	"time"
)

// MemoryDeduper is an in-process DeliveryDeduper for tests and single-process
// hosts. Entries expire lazily; when MaxEntries is reached the oldest expired
// entries are dropped first and, if the map is still full, FirstSeen fails
// closed with ErrDedupeFull (the Hub retries later).
type MemoryDeduper struct {
	// MaxEntries caps remembered ids (default 100000).
	MaxEntries int
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// ErrDedupeFull is returned by MemoryDeduper when it cannot remember more ids.
var ErrDedupeFull = errDedupeFull{}

type errDedupeFull struct{}

func (errDedupeFull) Error() string { return "support contract: dedupe store full" }

// FirstSeen implements DeliveryDeduper.
func (m *MemoryDeduper) FirstSeen(_ context.Context, id string, ttl time.Duration) (bool, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	t := now()
	max := m.MaxEntries
	if max <= 0 {
		max = 100000
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = make(map[string]time.Time)
	}
	if exp, ok := m.seen[id]; ok && t.Before(exp) {
		return false, nil
	}
	if len(m.seen) >= max {
		for k, exp := range m.seen {
			if !t.Before(exp) {
				delete(m.seen, k)
			}
		}
		if len(m.seen) >= max {
			return false, ErrDedupeFull
		}
	}
	m.seen[id] = t.Add(ttl)
	return true, nil
}
