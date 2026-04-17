package permission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
)

// fakeUser is a minimal AuthUser for tests that avoids pulling in the full
// BaseUser (no bcrypt, no GORM hooks). It satisfies the interface through
// direct fields.
type fakeUser struct {
	id    uuid.UUID
	orgID uuid.UUID
	email string
	role  string
}

func (u *fakeUser) GetID() uuid.UUID              { return u.id }
func (u *fakeUser) GetOrganizationID() uuid.UUID  { return u.orgID }
func (u *fakeUser) GetEmail() string              { return u.email }
func (u *fakeUser) GetRole() string               { return u.role }
func (u *fakeUser) GetPasswordHash() string       { return "" }
func (u *fakeUser) SetEmail(v string)             { u.email = v }
func (u *fakeUser) SetName(string)                {}
func (u *fakeUser) SetPasswordHash(string)        {}
func (u *fakeUser) SetRole(v string)              { u.role = v }
func (u *fakeUser) SetOrganizationID(v uuid.UUID) { u.orgID = v }

var _ modelbase.AuthUser = (*fakeUser)(nil)

func newSvc(t *testing.T, roleCaps map[Role][]Capability, userCaps map[uuid.UUID][]Capability) *Service {
	t.Helper()
	return New(Config{
		Store:    NewInMemoryStore(roleCaps, userCaps),
		CacheTTL: 50 * time.Millisecond,
	})
}

func TestNew_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = New(Config{})
}

func TestService_Check_RoleCap(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "admin"}
	svc := newSvc(t, map[Role][]Capability{
		RoleAdmin: {Cap("users", "create")},
	}, nil)

	if err := svc.Check(context.Background(), user, Cap("users", "create")); err != nil {
		t.Fatalf("Check: %v", err)
	}
	err := svc.Check(context.Background(), user, Cap("users", "delete"))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("want ErrPermissionDenied, got %v", err)
	}
}

func TestService_Check_UserOverride(t *testing.T) {
	userID := uuid.New()
	user := &fakeUser{id: userID, role: "agent"}
	svc := newSvc(t, nil, map[uuid.UUID][]Capability{
		userID: {Cap("reports", "export")},
	})
	if err := svc.Check(context.Background(), user, Cap("reports", "export")); err != nil {
		t.Fatalf("override Check: %v", err)
	}
}

func TestService_Check_SuperRoleBypass(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "owner"}
	svc := newSvc(t, nil, nil) // empty store, only role matters
	if err := svc.Check(context.Background(), user, Cap("anything", "goes")); err != nil {
		t.Fatalf("super role should bypass: %v", err)
	}
}

func TestService_Check_CustomSuperRoles(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "superadmin"}
	svc := New(Config{
		Store:      NewInMemoryStore(nil, nil),
		SuperRoles: []Role{Role("superadmin")},
	})
	if err := svc.Check(context.Background(), user, Cap("x", "y")); err != nil {
		t.Fatalf("custom super role: %v", err)
	}

	// Owner is no longer super because we replaced the list.
	owner := &fakeUser{id: uuid.New(), role: "owner"}
	if err := svc.Check(context.Background(), owner, Cap("x", "y")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("owner should NOT be super here, got %v", err)
	}
}

func TestService_Check_NilUser(t *testing.T) {
	svc := newSvc(t, nil, nil)
	if err := svc.Check(context.Background(), nil, Cap("x", "y")); !errors.Is(err, ErrNoUser) {
		t.Fatalf("want ErrNoUser, got %v", err)
	}
}

func TestService_Check_Wildcard(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "admin"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Wildcard}}, nil)
	if err := svc.Check(context.Background(), user, Cap("any", "thing")); err != nil {
		t.Fatalf("wildcard bypass: %v", err)
	}
}

func TestService_CheckAny(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "admin"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Cap("a", "b")}}, nil)

	if err := svc.CheckAny(context.Background(), user, Cap("x", "y"), Cap("a", "b")); err != nil {
		t.Fatalf("CheckAny hit: %v", err)
	}
	if err := svc.CheckAny(context.Background(), user, Cap("x", "y"), Cap("z", "w")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("want denied, got %v", err)
	}
	// No caps -> allowed.
	if err := svc.CheckAny(context.Background(), user); err != nil {
		t.Fatalf("CheckAny no caps: %v", err)
	}
}

func TestService_CheckAll(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "admin"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Cap("a", "b"), Cap("c", "d")}}, nil)

	if err := svc.CheckAll(context.Background(), user, Cap("a", "b"), Cap("c", "d")); err != nil {
		t.Fatalf("CheckAll: %v", err)
	}
	if err := svc.CheckAll(context.Background(), user, Cap("a", "b"), Cap("z", "z")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("want denied, got %v", err)
	}
}

func TestService_GetUserCapabilities_Dedup(t *testing.T) {
	userID := uuid.New()
	user := &fakeUser{id: userID, role: "admin"}
	svc := newSvc(t,
		map[Role][]Capability{RoleAdmin: {Cap("a", "b"), Cap("c", "d")}},
		map[uuid.UUID][]Capability{userID: {Cap("a", "b"), Cap("e", "f")}},
	)
	caps, err := svc.GetUserCapabilities(context.Background(), user)
	if err != nil {
		t.Fatalf("GetUserCapabilities: %v", err)
	}
	if len(caps) != 3 {
		t.Fatalf("want 3 deduped caps, got %d (%v)", len(caps), caps)
	}
}

func TestService_GetUserCapabilities_SuperRoleWildcard(t *testing.T) {
	user := &fakeUser{id: uuid.New(), role: "owner"}
	svc := newSvc(t, nil, nil)
	caps, err := svc.GetUserCapabilities(context.Background(), user)
	if err != nil {
		t.Fatalf("GetUserCapabilities: %v", err)
	}
	if len(caps) != 1 || caps[0] != Wildcard {
		t.Fatalf("want [*], got %v", caps)
	}
}

func TestService_Cache_TTLAndInvalidate(t *testing.T) {
	userID := uuid.New()
	user := &fakeUser{id: userID, role: "admin"}
	store := NewInMemoryStore(map[Role][]Capability{
		RoleAdmin: {Cap("a", "b")},
	}, nil)
	svc := New(Config{Store: store, CacheTTL: 20 * time.Millisecond})

	if err := svc.Check(context.Background(), user, Cap("a", "b")); err != nil {
		t.Fatalf("initial check: %v", err)
	}

	// Mutate store — cache should still hit until TTL elapses.
	store.SetRole(RoleAdmin, []Capability{Cap("z", "z")})
	if err := svc.Check(context.Background(), user, Cap("a", "b")); err != nil {
		t.Fatalf("cached hit expected: %v", err)
	}

	// InvalidateUser -> must see new grants.
	svc.InvalidateUser(userID)
	if err := svc.Check(context.Background(), user, Cap("a", "b")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("want denied after invalidate, got %v", err)
	}

	// Put grant back, cache again, then wait past TTL for natural expiry.
	store.SetRole(RoleAdmin, []Capability{Cap("a", "b")})
	svc.InvalidateUser(userID)
	if err := svc.Check(context.Background(), user, Cap("a", "b")); err != nil {
		t.Fatalf("post-refresh: %v", err)
	}
	store.SetRole(RoleAdmin, []Capability{Cap("w", "w")})
	time.Sleep(40 * time.Millisecond)
	if err := svc.Check(context.Background(), user, Cap("a", "b")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("want denied after TTL expiry, got %v", err)
	}
}

func TestService_InvalidateAll(t *testing.T) {
	userID := uuid.New()
	user := &fakeUser{id: userID, role: "admin"}
	store := NewInMemoryStore(map[Role][]Capability{RoleAdmin: {Cap("a", "b")}}, nil)
	svc := New(Config{Store: store, CacheTTL: time.Hour})

	_ = svc.Check(context.Background(), user, Cap("a", "b"))
	store.SetRole(RoleAdmin, []Capability{Cap("z", "z")})
	svc.InvalidateAll()
	if err := svc.Check(context.Background(), user, Cap("a", "b")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("InvalidateAll should have cleared cache, got %v", err)
	}
}

func TestService_ConcurrentAccess(t *testing.T) {
	userID := uuid.New()
	user := &fakeUser{id: userID, role: "admin"}
	svc := newSvc(t, map[Role][]Capability{RoleAdmin: {Cap("a", "b")}}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.Check(context.Background(), user, Cap("a", "b"))
			svc.InvalidateUser(userID)
		}()
	}
	wg.Wait()
}

func TestService_UnknownRoleIsNotFatal(t *testing.T) {
	// A user whose role has no grants in the store should simply be denied,
	// not error 500.
	user := &fakeUser{id: uuid.New(), role: "newbie"}
	svc := newSvc(t, map[Role][]Capability{
		RoleAdmin: {Cap("a", "b")},
	}, nil)

	err := svc.Check(context.Background(), user, Cap("a", "b"))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("want ErrPermissionDenied for unknown role, got %v", err)
	}
}

func TestCap_Helpers(t *testing.T) {
	if Cap("Users", "Create") != Capability("users.Create") {
		t.Fatalf("unexpected Cap result: %q", Cap("Users", "Create"))
	}
	c := Cap("users", "create")
	if c.Resource() != "users" || c.Action() != "create" {
		t.Fatalf("split: %s / %s", c.Resource(), c.Action())
	}
	if !Wildcard.Matches(Cap("x", "y")) {
		t.Fatal("wildcard should match anything")
	}
	if Cap("x", "y").Matches(Cap("x", "z")) {
		t.Fatal("non-wildcard should not cross-match")
	}
}
