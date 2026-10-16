package apitoken

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// scopeRe is the scope grammar: "*" or "<area>:<verb>" with "*" allowed as
// the verb ("catalog:*"). Matches apiroute.Principal.HasScope.
var scopeRe = regexp.MustCompile(`^(\*|[a-z][a-z0-9_]*:([a-z][a-z0-9_]*|\*))$`)

// lastUsedEvery throttles last_used_at writes per token.
const lastUsedEvery = time.Minute

// maxNameLen bounds APIToken.Name.
const maxNameLen = 120

// Service issues, authenticates, rotates and revokes tokens. Transport
// agnostic; safe for concurrent use. Construct it with NewService.
type Service struct {
	store       Store
	now         func() time.Time
	rand        io.Reader
	log         *slog.Logger
	publishable func(scope string) bool

	mu      sync.Mutex
	touched map[uuid.UUID]time.Time // last last_used_at write per token
}

// Option tunes a Service.
type Option func(*Service)

// WithClock overrides time.Now (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithRand overrides the entropy source (tests). Defaults to crypto/rand.
func WithRand(r io.Reader) Option {
	return func(s *Service) {
		if r != nil {
			s.rand = r
		}
	}
}

// WithLogger sets the logger for best-effort failures (last_used_at). Token
// plaintext is never logged. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.log = l
		}
	}
}

// WithPublishableScopes replaces the default publishable allowlist (any
// "<area>:read" plus "cart:write") with exactly these scopes.
func WithPublishableScopes(scopes ...string) Option {
	allowed := map[string]struct{}{}
	for _, sc := range scopes {
		allowed[sc] = struct{}{}
	}
	return func(s *Service) {
		s.publishable = func(scope string) bool {
			_, ok := allowed[scope]
			return ok
		}
	}
}

// DefaultPublishableScope is the default publishable allowlist: read scopes
// and "cart:write".
func DefaultPublishableScope(scope string) bool {
	if scope == "cart:write" {
		return true
	}
	area, verb, ok := strings.Cut(scope, ":")
	return ok && area != "*" && verb == "read"
}

// NewService builds a Service over store.
func NewService(store Store, opts ...Option) *Service {
	if store == nil {
		panic("apitoken: NewService requires a Store")
	}
	s := &Service{
		store:       store,
		now:         time.Now,
		rand:        defaultRand,
		log:         slog.Default(),
		publishable: DefaultPublishableScope,
		touched:     map[uuid.UUID]time.Time{},
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s
}

// IssueOption sets optional fields at Issue.
type IssueOption func(*APIToken)

// WithRateLimit sets RateLimitPerMinute (0 = the limiter's default).
func WithRateLimit(perMinute int) IssueOption {
	return func(t *APIToken) { t.RateLimitPerMinute = perMinute }
}

// WithCreatedBy records the user that issued the token.
func WithCreatedBy(userID uuid.UUID) IssueOption {
	return func(t *APIToken) {
		if userID != uuid.Nil {
			id := userID
			t.CreatedBy = &id
		}
	}
}

// Issue creates a token and returns its plaintext — the only time it is ever
// available — with the stored record. org must be uuid.Nil for KindPlatform
// and set for the other kinds. expiresAt nil means no expiry.
func (s *Service) Issue(ctx context.Context, org uuid.UUID, name string, kind Kind, scopes []string, expiresAt *time.Time, opts ...IssueOption) (string, *APIToken, error) {
	now := s.now().UTC()
	t := &APIToken{
		ID:        uuid.New(),
		Name:      strings.TrimSpace(name),
		Kind:      kind,
		Status:    StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if org != uuid.Nil {
		o := org
		t.OrganizationID = &o
	}
	if expiresAt != nil {
		e := expiresAt.UTC()
		t.ExpiresAt = &e
	}
	for _, o := range opts {
		if o != nil {
			o(t)
		}
	}
	norm, err := s.validate(t, scopes, now)
	if err != nil {
		return "", nil, err
	}
	t.Scopes = norm
	plain, err := s.create(ctx, t)
	if err != nil {
		return "", nil, err
	}
	return plain, t, nil
}

// validate checks kind/org, name, rate limit, expiry and scopes, returning
// the normalized (deduplicated, sorted) scopes.
func (s *Service) validate(t *APIToken, scopes []string, now time.Time) ([]string, error) {
	if !t.Kind.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, t.Kind)
	}
	if t.Kind == KindPlatform && t.OrganizationID != nil {
		return nil, fmt.Errorf("%w: platform tokens are not org-scoped", ErrInvalidKind)
	}
	if t.Kind != KindPlatform && t.OrganizationID == nil {
		return nil, fmt.Errorf("%w: %s tokens need an organization", ErrInvalidKind, t.Kind)
	}
	if t.Name == "" || utf8.RuneCountInString(t.Name) > maxNameLen {
		return nil, fmt.Errorf("%w: name must be 1-%d characters", ErrInvalidInput, maxNameLen)
	}
	if t.RateLimitPerMinute < 0 {
		return nil, fmt.Errorf("%w: rate limit must be >= 0", ErrInvalidInput)
	}
	if t.ExpiresAt != nil && !t.ExpiresAt.After(now) {
		return nil, fmt.Errorf("%w: expires_at is in the past", ErrInvalidInput)
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("%w: at least one scope is required", ErrInvalidScope)
	}
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		sc = strings.TrimSpace(sc)
		if !scopeRe.MatchString(sc) {
			return nil, fmt.Errorf("%w: %q must be <area>:<verb>", ErrInvalidScope, sc)
		}
		if t.Kind == KindPublishable && !s.publishable(sc) {
			return nil, fmt.Errorf("%w: %q is not allowed on a publishable token", ErrInvalidScope, sc)
		}
		out = append(out, sc)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// create generates the plaintext, stores the hash and returns the plaintext.
// A hash collision (astronomically unlikely) is retried once.
func (s *Service) create(ctx context.Context, t *APIToken) (string, error) {
	for attempt := 0; ; attempt++ {
		plain, err := generate(t.Kind, s.rand)
		if err != nil {
			return "", fmt.Errorf("apitoken: generate: %w", err)
		}
		t.TokenHash = HashToken(plain)
		t.Prefix = displayPrefix(plain)
		err = s.store.Create(ctx, t)
		if errors.Is(err, ErrDuplicateHash) && attempt == 0 {
			continue
		}
		if err != nil {
			return "", err
		}
		return plain, nil
	}
}

// Authenticate resolves a plaintext token. Malformed and unknown tokens are
// both ErrInvalidToken; a revoked one ErrRevoked; an expired one ErrExpired.
// On success last_used_at is refreshed at most once per minute per token
// (best effort: a failed write is logged, never returned).
func (s *Service) Authenticate(ctx context.Context, plaintext string) (*APIToken, error) {
	kind, ok := KindOf(plaintext)
	if !ok {
		return nil, ErrInvalidToken
	}
	t, err := s.store.GetByHash(ctx, HashToken(plaintext))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if t.Kind != kind {
		return nil, ErrInvalidToken
	}
	now := s.now().UTC()
	if err := t.Usable(now); err != nil {
		return nil, err
	}
	s.touch(ctx, t, now)
	return t, nil
}

// touch writes last_used_at when the last write is older than lastUsedEvery.
func (s *Service) touch(ctx context.Context, t *APIToken, now time.Time) {
	s.mu.Lock()
	last, seen := s.touched[t.ID]
	if !seen && t.LastUsedAt != nil {
		last, seen = *t.LastUsedAt, true
	}
	if seen && now.Sub(last) < lastUsedEvery {
		s.mu.Unlock()
		return
	}
	s.touched[t.ID] = now
	if len(s.touched) > 100_000 { // bound memory on hosts with huge token sets
		for id, at := range s.touched {
			if now.Sub(at) >= lastUsedEvery {
				delete(s.touched, id)
			}
		}
	}
	s.mu.Unlock()
	if err := s.store.TouchLastUsed(ctx, t.ID, now); err != nil {
		s.log.WarnContext(ctx, "apitoken: update last_used_at failed", "token_id", t.ID, "error", err)
		return
	}
	t.LastUsedAt = &now
}

// Rotate issues a replacement for token id with the same organization, name,
// kind, scopes, expiry and rate limit (RotatedFromID = id) and returns its
// plaintext. The old token keeps authenticating until now+grace (its
// ExpiresAt is pulled in, never extended); grace <= 0 revokes it at once.
func (s *Service) Rotate(ctx context.Context, id uuid.UUID, grace time.Duration) (string, *APIToken, error) {
	old, err := s.store.Get(ctx, id)
	if err != nil {
		return "", nil, err
	}
	now := s.now().UTC()
	if err := old.Usable(now); err != nil {
		return "", nil, err
	}
	from := old.ID
	nt := &APIToken{
		ID:                 uuid.New(),
		OrganizationID:     old.OrganizationID,
		Name:               old.Name,
		Kind:               old.Kind,
		Scopes:             slices.Clone(old.Scopes),
		Status:             StatusActive,
		ExpiresAt:          old.ExpiresAt,
		RotatedFromID:      &from,
		RateLimitPerMinute: old.RateLimitPerMinute,
		CreatedBy:          old.CreatedBy,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	plain, err := s.create(ctx, nt)
	if err != nil {
		return "", nil, err
	}
	if grace <= 0 {
		old.Status = StatusRevoked
	} else if until := now.Add(grace); old.ExpiresAt == nil || until.Before(*old.ExpiresAt) {
		old.ExpiresAt = &until
	}
	old.UpdatedAt = now
	if err := s.store.Update(ctx, old); err != nil {
		return "", nil, fmt.Errorf("apitoken: rotate: new token %s issued but old token not updated: %w", nt.ID, err)
	}
	return plain, nt, nil
}

// Revoke disables a token permanently. Revoking a revoked token is a no-op.
func (s *Service) Revoke(ctx context.Context, id uuid.UUID) error {
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status == StatusRevoked {
		return nil
	}
	t.Status = StatusRevoked
	t.UpdatedAt = s.now().UTC()
	return s.store.Update(ctx, t)
}

// Get returns one token.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*APIToken, error) {
	return s.store.Get(ctx, id)
}

// List returns the organization's tokens, newest first (uuid.Nil lists
// platform tokens).
func (s *Service) List(ctx context.Context, org uuid.UUID) ([]APIToken, error) {
	return s.store.List(ctx, org)
}
