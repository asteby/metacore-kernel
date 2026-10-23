package apitoken

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Store persists tokens. Implementations must enforce TokenHash uniqueness
// (ErrDuplicateHash) and return ErrNotFound for missing rows.
type Store interface {
	// Create inserts a token.
	Create(ctx context.Context, t *APIToken) error
	// Get loads a token by id.
	Get(ctx context.Context, id uuid.UUID) (*APIToken, error)
	// GetByHash loads a token by TokenHash.
	GetByHash(ctx context.Context, hash string) (*APIToken, error)
	// List returns the organization's tokens, newest first; uuid.Nil lists
	// platform tokens.
	List(ctx context.Context, org uuid.UUID) ([]APIToken, error)
	// Update saves every field of an existing token.
	Update(ctx context.Context, t *APIToken) error
	// TouchLastUsed sets last_used_at only.
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error
}

// ScopeStore is an optional Store capability: replace a token's scopes
// atomically, only while the token is usable. Service.SetScopes uses it when
// the Store implements it (MemoryStore and GormStore do); otherwise it falls
// back to Get + Update serialized by the Service (single process only).
type ScopeStore interface {
	// UpdateScopes sets scopes and updated_at on token id, touching nothing
	// else, provided the token is active (not revoked) and not expired at now.
	// It returns the stored token, ErrNotFound, ErrRevoked or ErrExpired.
	UpdateScopes(ctx context.Context, id uuid.UUID, scopes []string, now time.Time) (*APIToken, error)
}

// --- in-memory ------------------------------------------------------------

// MemoryStore is a goroutine-safe in-memory Store for tests and single-node
// tooling. Lost on restart.
type MemoryStore struct {
	mu     sync.RWMutex
	byID   map[uuid.UUID]APIToken
	byHash map[string]uuid.UUID
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[uuid.UUID]APIToken{}, byHash: map[string]uuid.UUID{}}
}

func cloneToken(t APIToken) *APIToken {
	t.Scopes = slices.Clone(t.Scopes)
	return &t
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, t *APIToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.byHash[t.TokenHash]; dup {
		return ErrDuplicateHash
	}
	if _, dup := s.byID[t.ID]; dup {
		return errors.New("apitoken: duplicate id")
	}
	s.byID[t.ID] = *cloneToken(*t)
	s.byHash[t.TokenHash] = t.ID
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (*APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneToken(t), nil
}

// GetByHash implements Store.
func (s *MemoryStore) GetByHash(_ context.Context, hash string) (*APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byHash[hash]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneToken(s.byID[id]), nil
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, org uuid.UUID) ([]APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []APIToken{}
	for _, t := range s.byID {
		if t.Org() == org {
			out = append(out, *cloneToken(t))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// Update implements Store.
func (s *MemoryStore) Update(_ context.Context, t *APIToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.byID[t.ID]
	if !ok {
		return ErrNotFound
	}
	if old.TokenHash != t.TokenHash {
		if _, dup := s.byHash[t.TokenHash]; dup {
			return ErrDuplicateHash
		}
		delete(s.byHash, old.TokenHash)
		s.byHash[t.TokenHash] = t.ID
	}
	s.byID[t.ID] = *cloneToken(*t)
	return nil
}

// UpdateScopes implements ScopeStore.
func (s *MemoryStore) UpdateScopes(_ context.Context, id uuid.UUID, scopes []string, now time.Time) (*APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	if err := t.Usable(now); err != nil {
		return nil, err
	}
	t.Scopes = slices.Clone(scopes)
	t.UpdatedAt = now
	s.byID[id] = t
	return cloneToken(t), nil
}

// TouchLastUsed implements Store.
func (s *MemoryStore) TouchLastUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	t.LastUsedAt = &at
	s.byID[id] = t
	return nil
}

// --- GORM -------------------------------------------------------------------

// GormStore is the durable Store over the metacore_api_tokens table
// (Postgres; SQLite for tests). Create the table with Migrate.
type GormStore struct {
	db *gorm.DB
}

var _ Store = (*GormStore)(nil)

// NewGormStore wraps db. It does not migrate; call Migrate (or own the DDL in
// the host's migrations).
func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

// Create implements Store.
func (s *GormStore) Create(ctx context.Context, t *APIToken) error {
	if err := s.db.WithContext(ctx).Create(t).Error; err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateHash
		}
		return err
	}
	return nil
}

// Get implements Store.
func (s *GormStore) Get(ctx context.Context, id uuid.UUID) (*APIToken, error) {
	return s.first(ctx, "id = ?", id)
}

// GetByHash implements Store.
func (s *GormStore) GetByHash(ctx context.Context, hash string) (*APIToken, error) {
	return s.first(ctx, "token_hash = ?", hash)
}

func (s *GormStore) first(ctx context.Context, where string, arg any) (*APIToken, error) {
	var t APIToken
	err := s.db.WithContext(ctx).Where(where, arg).Take(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// List implements Store.
func (s *GormStore) List(ctx context.Context, org uuid.UUID) ([]APIToken, error) {
	q := s.db.WithContext(ctx).Model(&APIToken{})
	if org == uuid.Nil {
		q = q.Where("organization_id IS NULL")
	} else {
		q = q.Where("organization_id = ?", org)
	}
	out := []APIToken{}
	if err := q.Order("created_at DESC").Order("id").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Update implements Store.
func (s *GormStore) Update(ctx context.Context, t *APIToken) error {
	res := s.db.WithContext(ctx).Model(&APIToken{}).Where("id = ?", t.ID).Select("*").Updates(t)
	if res.Error != nil {
		if isUniqueViolation(res.Error) {
			return ErrDuplicateHash
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateScopes implements ScopeStore. The usability check lives in the UPDATE's
// WHERE clause, so a concurrent Revoke cannot be overwritten.
func (s *GormStore) UpdateScopes(ctx context.Context, id uuid.UUID, scopes []string, now time.Time) (*APIToken, error) {
	res := s.db.WithContext(ctx).Model(&APIToken{}).
		Where("id = ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)", id, StatusActive, now).
		Select("scopes", "updated_at").
		UpdateColumns(&APIToken{Scopes: scopes, UpdatedAt: now})
	if res.Error != nil {
		return nil, res.Error
	}
	t, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected == 0 {
		if err := t.Usable(now); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	return t, nil
}

// TouchLastUsed implements Store.
func (s *GormStore) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	return s.db.WithContext(ctx).Model(&APIToken{}).Where("id = ?", id).
		UpdateColumn("last_used_at", at).Error
}

func isUniqueViolation(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "sqlstate 23505")
}
