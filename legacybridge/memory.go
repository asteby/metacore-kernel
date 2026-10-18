package legacybridge

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore implementa Store en memoria (pruebas y herramientas de un solo
// proceso). No es transaccional.
type MemoryStore struct {
	mu       sync.Mutex
	refs     map[string]Ref
	cursors  map[string]string
	rules    map[string]OwnershipRule
	phases   map[string]string
	pending  map[string]PendingEnvelope
	commands map[string]Command
	Now      func() time.Time
}

// NewMemoryStore crea un almacén vacío.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		refs: map[string]Ref{}, cursors: map[string]string{}, rules: map[string]OwnershipRule{},
		phases: map[string]string{}, pending: map[string]PendingEnvelope{}, commands: map[string]Command{},
		Now: time.Now,
	}
}

var _ Store = (*MemoryStore)(nil)

func k(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x00"
		}
		out += p
	}
	return out
}

func (m *MemoryStore) GetRef(_ context.Context, org, system, entity, ext string) (Ref, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refs[k(org, system, entity, ext)]
	return r, ok, nil
}

func (m *MemoryStore) GetRefByLocal(_ context.Context, org, system, entity, local string) (Ref, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.refs {
		if r.Org == org && r.System == system && r.Entity == entity && r.LocalID == local {
			return r, true, nil
		}
	}
	return Ref{}, false, nil
}

func (m *MemoryStore) PutRef(_ context.Context, r Ref) error {
	if err := r.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.refs {
		if o.Org == r.Org && o.System == r.System && o.Entity == r.Entity && o.LocalID == r.LocalID && o.ExternalID != r.ExternalID {
			return ErrRefConflict
		}
	}
	r = r.withDefaults()
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = m.Now().UTC()
	}
	m.refs[k(r.Org, r.System, r.Entity, r.ExternalID)] = r
	return nil
}

func (m *MemoryStore) GetCursor(_ context.Context, org, system, stream string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[k(org, system, stream)], nil
}

func (m *MemoryStore) SetCursor(_ context.Context, org, system, stream, cursor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursors[k(org, system, stream)] = cursor
	return nil
}

func (m *MemoryStore) Rules(_ context.Context, org, system string) ([]OwnershipRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OwnershipRule
	for _, r := range m.rules {
		if r.Org == org && r.System == system {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return k(out[i].Entity, out[i].Branch, out[i].Phase) < k(out[j].Entity, out[j].Branch, out[j].Phase)
	})
	return out, nil
}

func (m *MemoryStore) PutRule(_ context.Context, r OwnershipRule) error {
	r, err := r.Normalize()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r.UpdatedAt = m.Now().UTC()
	m.rules[k(r.Org, r.System, r.Entity, r.Branch, r.Phase)] = r
	return nil
}

func (m *MemoryStore) Phase(_ context.Context, org, system string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phases[k(org, system)], nil
}

func (m *MemoryStore) SetPhase(_ context.Context, org, system, phase, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phases[k(org, system)] = phase
	return nil
}

func (m *MemoryStore) PutPending(_ context.Context, p PendingEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[k(p.Org, p.System, p.Envelope.Entity, p.Envelope.ExternalID)] = p
	return nil
}

func (m *MemoryStore) GetPending(_ context.Context, org, system, entity, ext string) (PendingEnvelope, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[k(org, system, entity, ext)]
	return p, ok, nil
}

func (m *MemoryStore) DeletePending(_ context.Context, org, system, entity, ext string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, k(org, system, entity, ext))
	return nil
}

func (m *MemoryStore) DuePending(_ context.Context, now time.Time, limit int) ([]PendingEnvelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PendingEnvelope
	for _, p := range m.pending {
		if !p.NextAttempt.After(now) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextAttempt.Before(out[j].NextAttempt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) CountPending(_ context.Context, org, system string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, p := range m.pending {
		if p.Org == org && p.System == system {
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) InsertCommand(_ context.Context, c Command) (Command, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var seq int64
	for _, o := range m.commands {
		if o.Org == c.Org && o.System == c.System && o.IdempotencyKey == c.IdempotencyKey {
			return o, false, nil
		}
		if o.Org == c.Org && o.System == c.System && o.AggregateKey == c.AggregateKey && o.Seq > seq {
			seq = o.Seq
		}
	}
	now := m.Now().UTC()
	c.ID = uuid.NewString()
	c.Seq = seq + 1
	c.Status = CommandPending
	c.CreatedAt, c.UpdatedAt = now, now
	if c.NextAttemptAt.IsZero() {
		c.NextAttemptAt = now
	}
	m.commands[c.ID] = c
	return c, true, nil
}

func (m *MemoryStore) GetCommand(_ context.Context, id string) (Command, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.commands[id]
	return c, ok, nil
}

func (m *MemoryStore) HeadCommand(_ context.Context, a AggregateRef) (Command, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var head Command
	found := false
	for _, c := range m.commands {
		if c.Org != a.Org || c.System != a.System || c.AggregateKey != a.AggregateKey {
			continue
		}
		if c.Status != CommandPending && c.Status != CommandDead {
			continue
		}
		if !found || c.Seq < head.Seq {
			head, found = c, true
		}
	}
	return head, found, nil
}

func (m *MemoryStore) UpdateCommand(_ context.Context, c Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.commands[c.ID]; !ok {
		return ErrCommandNotFound
	}
	m.commands[c.ID] = c
	return nil
}

func (m *MemoryStore) ListCommands(_ context.Context, f CommandFilter) ([]Command, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Command
	for _, c := range m.commands {
		if (f.Org == "" || c.Org == f.Org) && (f.System == "" || c.System == f.System) && (f.Status == "" || c.Status == f.Status) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AggregateKey != out[j].AggregateKey {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Seq < out[j].Seq
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemoryStore) DueAggregates(ctx context.Context, now time.Time, limit int) ([]AggregateRef, error) {
	m.mu.Lock()
	seen := map[AggregateRef]bool{}
	for _, c := range m.commands {
		if c.Status == CommandPending {
			seen[AggregateRef{Org: c.Org, System: c.System, AggregateKey: c.AggregateKey}] = true
		}
	}
	m.mu.Unlock()
	var out []AggregateRef
	for a := range seen {
		head, ok, _ := m.HeadCommand(ctx, a)
		if ok && head.Status == CommandPending && !head.NextAttemptAt.After(now) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AggregateKey < out[j].AggregateKey })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) CountCommands(_ context.Context, org, system string, status CommandStatus) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, c := range m.commands {
		if c.Org == org && c.System == system && (status == "" || c.Status == status) {
			n++
		}
	}
	return n, nil
}
