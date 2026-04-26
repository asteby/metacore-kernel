package permission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
)

// Config configures Service. Only Store is mandatory; zero-values for the
// other fields fall back to sensible defaults documented on each field.
type Config struct {
	// Store is the backing PermissionStore. Required.
	Store PermissionStore

	// CacheTTL controls how long resolved capability sets are held in the
	// per-user cache. Zero (default) uses DefaultCacheTTL; a negative value
	// disables caching entirely (useful for tests).
	CacheTTL time.Duration

	// SuperRoles are roles whose members bypass every capability check. When
	// nil, DefaultSuperRoles() is used (which includes only RoleOwner).
	SuperRoles []Role
}

// DefaultCacheTTL is the cache TTL used when Config.CacheTTL is zero.
const DefaultCacheTTL = 5 * time.Minute

// Service is the framework-agnostic authorization entry point. It composes a
// PermissionStore with a small TTL cache and a SuperRoles policy.
//
// Service is safe for concurrent use. It does NOT import Fiber — the Fiber
// middleware lives in middleware.go so the service can be driven from gRPC,
// Echo, or a CLI without pulling in HTTP types.
type Service struct {
	store      PermissionStore
	cache      *capCache
	superRoles map[Role]struct{}
}

// New constructs a Service. Panics if Config.Store is nil; authorization is
// security-critical and a misconfigured boot should fail loudly rather than
// silently allow every request.
func New(config Config) *Service {
	if config.Store == nil {
		panic(ErrNilStore)
	}

	ttl := config.CacheTTL
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}

	superRoles := config.SuperRoles
	if superRoles == nil {
		superRoles = DefaultSuperRoles()
	}
	superSet := make(map[Role]struct{}, len(superRoles))
	for _, r := range superRoles {
		superSet[NormalizeRole(r.String())] = struct{}{}
	}

	return &Service{
		store:      config.Store,
		cache:      newCapCache(ttl),
		superRoles: superSet,
	}
}

// Check returns nil iff user holds the capability cap. Returns ErrNoUser
// when user is nil, ErrPermissionDenied otherwise.
func (s *Service) Check(ctx context.Context, user modelbase.AuthUser, cap Capability) error {
	if user == nil {
		return ErrNoUser
	}

	// SuperRole fast-path: skip the store/cache entirely.
	if s.isSuperRole(user.GetRole()) {
		return nil
	}

	caps, err := s.GetUserCapabilities(ctx, user)
	if err != nil {
		return err
	}

	for _, c := range caps {
		if c.Matches(cap) {
			return nil
		}
	}

	return fmt.Errorf("%w: missing capability %q", ErrPermissionDenied, cap)
}

// CheckAny returns nil iff user holds at least one of caps. With zero caps,
// returns nil (treat "no requirements" as "allowed") — matches how the
// handler layer typically degrades to an auth-only gate.
func (s *Service) CheckAny(ctx context.Context, user modelbase.AuthUser, caps ...Capability) error {
	if user == nil {
		return ErrNoUser
	}
	if len(caps) == 0 {
		return nil
	}
	if s.isSuperRole(user.GetRole()) {
		return nil
	}

	userCaps, err := s.GetUserCapabilities(ctx, user)
	if err != nil {
		return err
	}
	for _, want := range caps {
		for _, have := range userCaps {
			if have.Matches(want) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: missing any of %v", ErrPermissionDenied, caps)
}

// CheckAll returns nil iff user holds every capability in caps. With zero
// caps, returns nil.
func (s *Service) CheckAll(ctx context.Context, user modelbase.AuthUser, caps ...Capability) error {
	if user == nil {
		return ErrNoUser
	}
	if len(caps) == 0 {
		return nil
	}
	if s.isSuperRole(user.GetRole()) {
		return nil
	}

	userCaps, err := s.GetUserCapabilities(ctx, user)
	if err != nil {
		return err
	}

	set := make(map[Capability]struct{}, len(userCaps))
	hasWildcard := false
	for _, c := range userCaps {
		if c == Wildcard {
			hasWildcard = true
			break
		}
		set[c] = struct{}{}
	}
	if hasWildcard {
		return nil
	}

	for _, want := range caps {
		if _, ok := set[want]; !ok {
			return fmt.Errorf("%w: missing %q", ErrPermissionDenied, want)
		}
	}
	return nil
}

// GetUserCapabilities returns the deduplicated list of capabilities granted
// to the user (via role + per-user overrides). Mainly useful for frontends
// that pre-render UI based on what the user can do.
//
// The returned slice is freshly allocated; callers may sort/mutate it
// without affecting the cache.
func (s *Service) GetUserCapabilities(ctx context.Context, user modelbase.AuthUser) ([]Capability, error) {
	if user == nil {
		return nil, ErrNoUser
	}

	userID := user.GetID()
	if cached, ok := s.cache.get(userID); ok {
		out := make([]Capability, len(cached))
		copy(out, cached)
		return out, nil
	}

	// SuperRole short-circuit — surface as a single Wildcard entry so
	// GetUserCapabilities callers (e.g. a frontend) render correctly.
	if s.isSuperRole(user.GetRole()) {
		caps := []Capability{Wildcard}
		s.cache.put(userID, caps)
		out := make([]Capability, len(caps))
		copy(out, caps)
		return out, nil
	}

	seen := make(map[Capability]struct{}, 16)
	out := make([]Capability, 0, 16)

	// Role grants.
	if role := NormalizeRole(user.GetRole()); role != "" {
		roleCaps, err := s.store.GetRolePermissions(ctx, role)
		if err != nil && !errors.Is(err, ErrUnknownRole) {
			return nil, err
		}
		for _, c := range roleCaps {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}

	// Per-user overrides (additive).
	if userID != uuid.Nil {
		userCaps, err := s.store.GetUserPermissions(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, c := range userCaps {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}

	s.cache.put(userID, out)

	// Return a copy so the caller cannot mutate the cached slice.
	ret := make([]Capability, len(out))
	copy(ret, out)
	return ret, nil
}

// InvalidateUser drops the cached capability list for userID. Call this when
// a user's role or per-user grants change.
func (s *Service) InvalidateUser(userID uuid.UUID) { s.cache.invalidate(userID) }

// InvalidateAll drops every cached entry. Call this when role->capability
// mappings change, since those affect many users at once.
func (s *Service) InvalidateAll() { s.cache.invalidateAll() }

// isSuperRole reports whether the given role string is in the configured
// SuperRoles set. Lookup is on the normalized form so casing cannot be used
// to bypass the policy.
func (s *Service) isSuperRole(role string) bool {
	if role == "" {
		return false
	}
	_, ok := s.superRoles[NormalizeRole(role)]
	return ok
}
