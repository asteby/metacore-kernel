package batch

import (
	"context"
	"sync"
)

// MemoryLedger is an in-memory Ledger for tests and single-process tools.
type MemoryLedger struct {
	mu   sync.Mutex
	recs map[Scope][]Record
}

// NewMemoryLedger returns an empty MemoryLedger.
func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{recs: map[Scope][]Record{}} }

func (m *MemoryLedger) Done(_ context.Context, scope Scope, manifestSHA string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs[scope] {
		if r.ManifestSHA == manifestSHA {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryLedger) LastContentPrint(_ context.Context, scope Scope) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.recs[scope]
	if len(rs) == 0 {
		return "", nil
	}
	return rs[len(rs)-1].ContentPrint, nil
}

func (m *MemoryLedger) Record(_ context.Context, scope Scope, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs[scope] {
		if r.ManifestSHA == rec.ManifestSHA {
			return nil // idempotent
		}
	}
	m.recs[scope] = append(m.recs[scope], rec)
	return nil
}

// Records returns a copy of what was recorded for scope, oldest first.
func (m *MemoryLedger) Records(scope Scope) []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.recs[scope]...)
}
