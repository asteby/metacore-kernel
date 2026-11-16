package agent

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/vector"
)

// Layered conversation memory for the copilot (CopilotConfig.LayeredMemory).
//
// Instead of re-sending the last N raw messages, every turn sees:
//
//  1. a recent window: the last few exchanges verbatim (MemoryConfig.RecentExchanges);
//  2. a conversation index: one line per older turn ("#14 · título → resultado"),
//     sent as the opening message of the history so it stays in the cached prefix;
//  3. recall on demand: the recall_turn tool returns an older turn in full,
//     with the facts its tools produced;
//  4. long-term memory: durable facts per user+org (remember_fact/forget_fact),
//     rendered in the turn context.
//
// Titles, summaries and facts come from the model's own final_answer (optional
// fields), with a deterministic fallback; tool digests are computed from the
// tool results. No extra LLM call is ever made.

// TurnRecord is the memory of one finished copilot turn.
type TurnRecord struct {
	SessionID      uuid.UUID
	OrganizationID uuid.UUID
	UserID         uuid.UUID
	// Turn is the 1-based number of the turn within the session.
	Turn int
	// Title (<= 8 words) and Summary (<= 20 words) feed the index line.
	Title   string
	Summary string
	// Facts are short statements the model chose to keep from the turn.
	Facts []string
	// ToolDigests are one-line deterministic digests of the turn's tool calls
	// (tool, key arguments, row count / key numbers).
	ToolDigests []string
	// UserMessage and Answer are the turn's texts (capped), for recall.
	UserMessage string
	Answer      string
	// Embedding is the vector of UserMessage when an Embedder is configured.
	Embedding []float32
	CreatedAt time.Time
}

// MemoryFact is a durable fact or preference of a user within an org.
type MemoryFact struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	UserID         uuid.UUID
	Text           string
	CreatedAt      time.Time
}

// TurnMemoryStore persists layered memory. Implementations must be safe for
// concurrent use. NewInMemoryTurnStore is process-local; NewGormTurnStore
// persists to SQL.
type TurnMemoryStore interface {
	// SaveTurn stores a finished turn (one row per session and turn number).
	SaveTurn(ctx context.Context, rec TurnRecord) error
	// ListTurns returns a session's turns ordered by Turn ascending.
	ListTurns(ctx context.Context, sessionID uuid.UUID) ([]TurnRecord, error)
	// ListFacts returns a user's durable facts ordered by CreatedAt, then ID.
	ListFacts(ctx context.Context, orgID, userID uuid.UUID) ([]MemoryFact, error)
	// SaveFact stores a durable fact.
	SaveFact(ctx context.Context, f MemoryFact) error
	// DeleteFact removes a durable fact of that user.
	DeleteFact(ctx context.Context, orgID, userID, id uuid.UUID) error
}

// MemoryConfig tunes layered memory. Every zero value takes its default.
type MemoryConfig struct {
	// Store persists turns and facts. Default: a process-local in-memory
	// store (memory is lost on restart; use NewGormTurnStore in production).
	Store TurnMemoryStore
	// Embedder enables semantic prefetch (for example bge-m3). Optional: the
	// lexical fallback is used without it or when it fails.
	Embedder vector.Embedder
	// RecentExchanges is the minimum number of earlier exchanges sent
	// verbatim (default 3). The window advances in steps of this size, so it
	// holds between RecentExchanges and 2*RecentExchanges-1 exchanges and the
	// sent history is append-only between steps (provider prefix caching).
	RecentExchanges int
	// IndexTokenBudget caps the conversation index (default 800 tokens, about
	// 25-30 lines; older lines are dropped first and stay reachable through
	// recall_turn).
	IndexTokenBudget int
	// MaxFacts caps the durable facts per user+org (default 20).
	MaxFacts int
	// FactsTokenBudget caps the rendered durable facts (default 300 tokens).
	FactsTokenBudget int
	// TurnDataTokenBudget caps the facts and tool digests of the previous
	// turn repeated in the turn context (default 200 tokens).
	TurnDataTokenBudget int
	// PrefetchThreshold is the cosine similarity (with an Embedder) above
	// which an older turn is attached to the turn context (default 0.75).
	PrefetchThreshold float64
	// LexicalPrefetchThreshold is the share of the message's significant
	// words an older turn must contain without an Embedder (default 0.5,
	// with at least two shared words).
	LexicalPrefetchThreshold float64
	// RecallMaxBytes caps what recall_turn returns (default 6000).
	RecallMaxBytes int
}

// Layered memory defaults (see MemoryConfig).
const (
	DefaultMemoryRecentExchanges   = 3
	DefaultMemoryIndexTokenBudget  = 800
	DefaultMemoryMaxFacts          = 20
	DefaultMemoryFactsTokenBudget  = 300
	DefaultMemoryTurnDataBudget    = 200
	DefaultMemoryPrefetchThreshold = 0.75
	DefaultMemoryLexicalThreshold  = 0.5
	DefaultMemoryRecallMaxBytes    = 6000
)

// withDefaults fills the zero values.
func (c MemoryConfig) withDefaults() MemoryConfig {
	if c.Store == nil {
		c.Store = NewInMemoryTurnStore()
	}
	if c.RecentExchanges <= 0 {
		c.RecentExchanges = DefaultMemoryRecentExchanges
	}
	if c.IndexTokenBudget <= 0 {
		c.IndexTokenBudget = DefaultMemoryIndexTokenBudget
	}
	if c.MaxFacts <= 0 {
		c.MaxFacts = DefaultMemoryMaxFacts
	}
	if c.FactsTokenBudget <= 0 {
		c.FactsTokenBudget = DefaultMemoryFactsTokenBudget
	}
	if c.TurnDataTokenBudget <= 0 {
		c.TurnDataTokenBudget = DefaultMemoryTurnDataBudget
	}
	if c.PrefetchThreshold <= 0 {
		c.PrefetchThreshold = DefaultMemoryPrefetchThreshold
	}
	if c.LexicalPrefetchThreshold <= 0 {
		c.LexicalPrefetchThreshold = DefaultMemoryLexicalThreshold
	}
	if c.RecallMaxBytes <= 0 {
		c.RecallMaxBytes = DefaultMemoryRecallMaxBytes
	}
	return c
}

// windowExchanges is how many earlier exchanges the recent window keeps once
// n turns are done: it starts at a multiple of step, so it only moves every
// step turns (step..2*step-1 exchanges).
func windowExchanges(n, step int) int {
	if step <= 0 {
		step = DefaultMemoryRecentExchanges
	}
	if n <= step {
		return step
	}
	return step + (n-step)%step
}

// InMemoryTurnStore is the process-local TurnMemoryStore.
type InMemoryTurnStore struct {
	mu    sync.RWMutex
	turns map[uuid.UUID][]TurnRecord
	facts map[[2]uuid.UUID][]MemoryFact
}

// NewInMemoryTurnStore builds an empty in-memory store.
func NewInMemoryTurnStore() *InMemoryTurnStore {
	return &InMemoryTurnStore{
		turns: map[uuid.UUID][]TurnRecord{},
		facts: map[[2]uuid.UUID][]MemoryFact{},
	}
}

// SaveTurn stores rec, replacing a turn with the same number.
func (s *InMemoryTurnStore) SaveTurn(_ context.Context, rec TurnRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.turns[rec.SessionID]
	for i := range list {
		if list[i].Turn == rec.Turn {
			list[i] = rec
			return nil
		}
	}
	list = append(list, rec)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Turn < list[j].Turn })
	s.turns[rec.SessionID] = list
	return nil
}

// ListTurns returns a copy of the session's turns.
func (s *InMemoryTurnStore) ListTurns(_ context.Context, sessionID uuid.UUID) ([]TurnRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]TurnRecord(nil), s.turns[sessionID]...), nil
}

// ListFacts returns a copy of the user's facts in stable order.
func (s *InMemoryTurnStore) ListFacts(_ context.Context, orgID, userID uuid.UUID) ([]MemoryFact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]MemoryFact(nil), s.facts[[2]uuid.UUID{orgID, userID}]...)
	sortFacts(out)
	return out, nil
}

// SaveFact stores f, replacing a fact with the same ID.
func (s *InMemoryTurnStore) SaveFact(_ context.Context, f MemoryFact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]uuid.UUID{f.OrganizationID, f.UserID}
	list := s.facts[key]
	for i := range list {
		if list[i].ID == f.ID {
			list[i] = f
			return nil
		}
	}
	s.facts[key] = append(list, f)
	return nil
}

// DeleteFact removes the fact; a missing fact is not an error.
func (s *InMemoryTurnStore) DeleteFact(_ context.Context, orgID, userID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]uuid.UUID{orgID, userID}
	list := s.facts[key]
	for i := range list {
		if list[i].ID == id {
			s.facts[key] = append(list[:i:i], list[i+1:]...)
			return nil
		}
	}
	return nil
}

func sortFacts(fs []MemoryFact) {
	sort.SliceStable(fs, func(i, j int) bool {
		if !fs[i].CreatedAt.Equal(fs[j].CreatedAt) {
			return fs[i].CreatedAt.Before(fs[j].CreatedAt)
		}
		return strings.Compare(fs[i].ID.String(), fs[j].ID.String()) < 0
	})
}
