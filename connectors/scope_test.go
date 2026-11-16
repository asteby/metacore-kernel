package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// scopedMapStore resolves branch → entity → organization over in-memory maps,
// the same walk a real host store does. strict makes the entity level refuse to
// inherit the organization's credentials.
type scopedMapStore struct {
	org      map[string]map[string]string
	entity   map[uuid.UUID]map[string]map[string]string
	branch   map[uuid.UUID]map[string]map[string]string
	strict   map[string]bool
	getCalls int
}

func (s *scopedMapStore) Get(_ context.Context, _ uuid.UUID, key string) (map[string]string, bool, error) {
	s.getCalls++
	c, ok := s.org[key]
	return c, ok, nil
}

func (s *scopedMapStore) GetScoped(_ context.Context, _ uuid.UUID, key string, scope Scope) (map[string]string, bool, error) {
	if scope.BranchID != uuid.Nil {
		if c, ok := s.branch[scope.BranchID][key]; ok {
			return c, true, nil
		}
	}
	if scope.EntityID != uuid.Nil {
		if c, ok := s.entity[scope.EntityID][key]; ok {
			return c, true, nil
		}
		if s.strict[key] {
			return nil, false, ErrScopedConnectorRequired
		}
	}
	c, ok := s.org[key]
	return c, ok, nil
}

func TestScopeZeroAndString(t *testing.T) {
	if !(Scope{}).IsZero() {
		t.Fatal("zero scope must be IsZero")
	}
	e, b := uuid.New(), uuid.New()
	if (Scope{EntityID: e}).IsZero() || (Scope{BranchID: b}).IsZero() {
		t.Fatal("a scope with an id is not zero")
	}
	if got := (Scope{}).String(); got != "org" {
		t.Fatalf("zero scope renders %q, want org", got)
	}
}

func TestScopeContextRoundTrip(t *testing.T) {
	if got := ScopeFrom(context.Background()); !got.IsZero() {
		t.Fatalf("empty context must give the zero scope, got %v", got)
	}
	var nilCtx context.Context
	if got := ScopeFrom(nilCtx); !got.IsZero() {
		t.Fatalf("nil context must give the zero scope, got %v", got)
	}
	s := Scope{EntityID: uuid.New(), BranchID: uuid.New()}
	if got := ScopeFrom(WithScope(context.Background(), s)); got != s {
		t.Fatalf("round trip: got %v want %v", got, s)
	}
	// A zero scope leaves the context as it was.
	parent := WithScope(context.Background(), s)
	if got := ScopeFrom(WithScope(parent, Scope{})); got != s {
		t.Fatalf("WithScope(zero) must not clear the parent scope, got %v", got)
	}
}

func TestGetScopedInheritsBranchEntityOrg(t *testing.T) {
	org, e1, e2, b1 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	st := &scopedMapStore{
		org:    map[string]map[string]string{"mercadopago": {"access_token": "ORG"}},
		entity: map[uuid.UUID]map[string]map[string]string{e1: {"mercadopago": {"access_token": "E1"}}},
		branch: map[uuid.UUID]map[string]map[string]string{b1: {"mercadopago": {"access_token": "B1"}}},
	}
	r := NewResolver(st)
	ctx := context.Background()
	cases := []struct {
		name  string
		scope Scope
		want  string
	}{
		{"branch wins over entity and org", Scope{EntityID: e1, BranchID: b1}, "B1"},
		{"entity wins over org", Scope{EntityID: e1}, "E1"},
		{"entity without its own falls back to org when inherit is allowed", Scope{EntityID: e2}, "ORG"},
		{"zero scope is the organization", Scope{}, "ORG"},
	}
	for _, c := range cases {
		creds, err := r.GetScoped(ctx, org, "mercadopago", c.scope)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if creds["access_token"] != c.want {
			t.Fatalf("%s: got %q want %q", c.name, creds["access_token"], c.want)
		}
	}
}

func TestGetScopedStrictEntityNeverUsesAnotherEntitysCredentials(t *testing.T) {
	org, e1, e2 := uuid.New(), uuid.New(), uuid.New()
	st := &scopedMapStore{
		org:    map[string]map[string]string{"mercadopago": {"access_token": "ORG"}},
		entity: map[uuid.UUID]map[string]map[string]string{e1: {"mercadopago": {"access_token": "E1"}}},
		strict: map[string]bool{"mercadopago": true},
	}
	r := NewResolver(st)
	ctx := context.Background()
	// e2 has no connector of its own and payments are strict: it must fail, and
	// it must not receive e1's token nor the organization's.
	creds, err := r.GetScoped(ctx, org, "mercadopago", Scope{EntityID: e2})
	if !errors.Is(err, ErrScopedConnectorRequired) {
		t.Fatalf("want ErrScopedConnectorRequired, got creds=%v err=%v", creds, err)
	}
	if creds != nil {
		t.Fatalf("a refused lookup must not leak credentials, got %v", creds)
	}
	// e1 keeps resolving its own.
	got, err := r.GetScoped(ctx, org, "mercadopago", Scope{EntityID: e1})
	if err != nil || got["access_token"] != "E1" {
		t.Fatalf("e1 must resolve its own token, got %v err=%v", got, err)
	}
}

func TestGetScopedOverPlainStoreIsGet(t *testing.T) {
	org := uuid.New()
	r := NewResolver(newStore(org)) // mapStore does not implement ScopedStore
	creds, err := r.GetScoped(context.Background(), org, "github", Scope{EntityID: uuid.New()})
	if err != nil || creds["access_token"] != "ghp_x" {
		t.Fatalf("a Store without scoping must behave as Get, got %v err=%v", creds, err)
	}
}

func TestGetScopedNotFoundAndOff(t *testing.T) {
	org := uuid.New()
	st := &scopedMapStore{org: map[string]map[string]string{}}
	r := NewResolver(st)
	if _, err := r.GetScoped(context.Background(), org, "slack", Scope{EntityID: uuid.New()}); !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("want ErrConnectorNotFound, got %v", err)
	}
	var off *Resolver
	if _, err := off.GetScoped(context.Background(), org, "slack", Scope{EntityID: uuid.New()}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil resolver must be the off state, got %v", err)
	}
}
