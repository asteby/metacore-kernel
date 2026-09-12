package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
	"github.com/asteby/metacore-kernel/query"
)

// denyAllStore is a PermissionStore that grants NOTHING to anyone — the
// strictest possible host configuration, used to prove the system caller
// bypasses the gate rather than merely happening to hold a broad capability.
type denyAllStore struct{}

func (denyAllStore) GetRolePermissions(ctx context.Context, role permission.Role) ([]permission.Capability, error) {
	return nil, nil
}

func (denyAllStore) GetUserPermissions(ctx context.Context, userID uuid.UUID) ([]permission.Capability, error) {
	return nil, nil
}

func serviceWithDenyAllPerms(t *testing.T) *Service {
	t.Helper()
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	perms := permission.New(permission.Config{
		Store:      denyAllStore{},
		CacheTTL:   -1,
		SuperRoles: []permission.Role{}, // no super roles either — a real dead end for a normal user
	})
	return New(Config{
		DB:                 db,
		Metadata:           metadata.New(metadata.Config{CacheTTL: -1}),
		Permissions:        perms,
		RequirePermissions: true,
	})
}

// TestSystemCallerBypassesPermissionGate proves (a): a real user is denied by
// the strictest possible permission config, but a NewSystemCaller principal
// sails through every action untouched.
func TestSystemCallerBypassesPermissionGate(t *testing.T) {
	svc := serviceWithDenyAllPerms(t)
	orgID := uuid.New()

	realUser := &fakeUser{id: uuid.New(), orgID: orgID, role: "member"}
	for _, action := range []string{"read", "create", "update", "delete"} {
		if err := svc.checkPerm(context.Background(), realUser, "test_products", action); !errors.Is(err, permission.ErrPermissionDenied) {
			t.Errorf("real user, action %s: want ErrPermissionDenied, got %v", action, err)
		}
	}

	sysUser := NewSystemCaller(orgID)
	for _, action := range []string{"read", "create", "update", "delete"} {
		if err := svc.checkPerm(context.Background(), sysUser, "test_products", action); err != nil {
			t.Errorf("system caller, action %s: want nil (bypass), got %v", action, err)
		}
	}
}

// TestSystemCallerEndToEndBypassesPermsButKeepsTenantScoping exercises the
// bypass through the real Create/Get/List surface (not just checkPerm) and
// proves (b): tenant scoping is NOT bypassed — a system caller scoped to
// orgA can never read orgB's rows, and its writes land tagged with orgA.
func TestSystemCallerEndToEndBypassesPermsButKeepsTenantScoping(t *testing.T) {
	svc := serviceWithDenyAllPerms(t)
	orgA := uuid.New()
	orgB := uuid.New()

	sysA := NewSystemCaller(orgA)
	sysB := NewSystemCaller(orgB)

	// A real user would be denied create by denyAllStore; the system caller
	// must succeed, proving the perms gate really is skipped end to end.
	created, err := svc.Create(context.Background(), "test_products", sysA, map[string]any{
		"name":  "widget",
		"price": 9.99,
	})
	if err != nil {
		t.Fatalf("system caller create: %v", err)
	}
	if got := created["organization_id"]; got != orgA.String() {
		t.Errorf("row stamped with organization_id %v, want %v", got, orgA)
	}

	id, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	// Same org: the system caller (and a real one, if perms allowed it) can
	// read it back.
	if _, err := svc.Get(context.Background(), "test_products", sysA, id); err != nil {
		t.Errorf("sysA get own-org row: %v", err)
	}

	// Cross-org: a system caller scoped to orgB must NOT see orgA's row —
	// tenant scoping is independent of, and unaffected by, the perms bypass.
	if _, err := svc.Get(context.Background(), "test_products", sysB, id); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("sysB cross-org get: want ErrRecordNotFound, got %v", err)
	}

	list, _, err := svc.List(context.Background(), "test_products", sysB, query.Params{})
	if err != nil {
		t.Fatalf("sysB list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("sysB list leaked %d cross-org row(s)", len(list))
	}
}

// TestSystemCallerCannotBeForgedByRoleString guards the design decision that
// the bypass is gated on the concrete unexported type, not on GetRole() ==
// SystemRole — a real AuthUser implementation that happens to claim the
// "system" role (e.g. a compromised or misconfigured token) must still be
// denied like any other unprivileged caller.
func TestSystemCallerCannotBeForgedByRoleString(t *testing.T) {
	svc := serviceWithDenyAllPerms(t)
	orgID := uuid.New()

	forged := &fakeUser{id: uuid.New(), orgID: orgID, role: SystemRole}
	if err := svc.checkPerm(context.Background(), forged, "test_products", "create"); !errors.Is(err, permission.ErrPermissionDenied) {
		t.Errorf("forged role=%q user: want ErrPermissionDenied, got %v", SystemRole, err)
	}
}

// TestNormalHTTPPathUnaffectedByPermissive verifies the pre-existing
// fail-open/fail-closed behaviour for regular AuthUser callers is completely
// unchanged by the system-caller addition (c): no regressions to the normal
// path when Permissions IS wired and allows the caller.
func TestNormalHTTPPathUnaffectedByPermissive(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	svc := New(Config{DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1})})

	user := newUser(uuid.New())
	if err := svc.checkPerm(context.Background(), user, "test_products", "create"); err != nil {
		t.Errorf("legacy no-perms-wired behaviour changed: %v", err)
	}
	out := createProduct(t, svc, user, "widget", 1.0)
	if out["organization_id"] != user.orgID.String() {
		t.Errorf("normal user create scoping regressed: got %v", out["organization_id"])
	}
}
