package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// These tests pin the authorization posture of a Service built WITHOUT a
// permission service. The historical behaviour is to allow everything, which
// is a fail-OPEN default that no log or error announced — a host that simply
// forgot to wire authz served every dynamic endpoint wide open. The kernel now
// lets a host opt into failing closed, and that choice must be honoured on
// every action, not just reads.

func serviceWithoutPerms(t *testing.T, requirePerms bool) *Service {
	t.Helper()
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	return New(Config{
		DB:                 db,
		Metadata:           metadata.New(metadata.Config{CacheTTL: -1}),
		RequirePermissions: requirePerms,
	})
}

func TestCheckPermDeniesWhenTheHostRequiresPermissions(t *testing.T) {
	svc := serviceWithoutPerms(t, true)

	for _, action := range []string{"read", "create", "update", "delete"} {
		err := svc.checkPerm(context.Background(), nil, "test_products", action)
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: want a denial wrapping ErrForbidden, got %v", action, err)
		}
		if !errors.Is(err, ErrPermissionServiceMissing) {
			t.Errorf("%s: the cause should be identifiable, got %v", action, err)
		}
	}
}

func TestCheckPermKeepsAllowingWhenTheHostDidNotOptIn(t *testing.T) {
	svc := serviceWithoutPerms(t, false)

	// The legacy posture: hosts that authorize in their own handler layer keep
	// working untouched. New() warns about it at construction.
	if err := svc.checkPerm(context.Background(), nil, "test_products", "delete"); err != nil {
		t.Errorf("legacy behaviour must be preserved, got %v", err)
	}
}

// TestPermissionDenialIsForbiddenNot500 guards the HTTP mapping: the denial
// wraps ErrForbidden, and the handler's error switch compared errors by
// identity, so a wrapped denial used to fall through to 500. A denial reported
// as a server error reads as "our bug" rather than "not allowed".
func TestPermissionDenialMapsToForbidden(t *testing.T) {
	if !errors.Is(ErrPermissionServiceMissing, ErrForbidden) {
		t.Fatal("ErrPermissionServiceMissing must wrap ErrForbidden so it maps to 403")
	}
}

// orgScopedModel carries organization_id, so tenant scoping APPLIES to it.
type orgScopedModel struct {
	modelbase.BaseUUIDModel
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
}

func (orgScopedModel) TableName() string { return "org_scoped_things" }
func (orgScopedModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Things"}
}
func (orgScopedModel) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Thing"}
}

// globalModel has no organization_id: nothing to isolate.
type globalModel struct {
	modelbase.BaseUUIDModel
	Code string `json:"code"`
}

func (globalModel) TableName() string { return "global_things" }
func (globalModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Global"}
}
func (globalModel) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Global"}
}

func TestScopeOrDenyRefusesWhenItCannotIsolateTheTenant(t *testing.T) {
	svc := serviceWithoutPerms(t, false)
	svc.requireTenant = true
	db := svc.db

	// An org-scoped model read with NO user: this is the cross-tenant leak the
	// old `user != nil && hasOrgColumn(...)` guard let through in silence.
	if _, err := svc.scopeOrDeny(db, &orgScopedModel{}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("an unscoped read of an org model must be refused, got %v", err)
	}

	// A model with no organization_id at all — for a host that declared
	// everything must be scoped, that is a declaration mistake, not a licence
	// to read globally.
	if _, err := svc.scopeOrDeny(db, &globalModel{}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("a model with no org column must be refused under RequireTenantScope, got %v", err)
	}
}

func TestScopeOrDenyKeepsTheLegacyBehaviourByDefault(t *testing.T) {
	svc := serviceWithoutPerms(t, false) // requireTenant stays false
	db := svc.db

	for _, model := range []any{&orgScopedModel{}, &globalModel{}} {
		got, err := svc.scopeOrDeny(db, model, nil)
		if err != nil {
			t.Errorf("%T: default posture must not break existing hosts, got %v", model, err)
		}
		if got == nil {
			t.Errorf("%T: a usable query is expected", model)
		}
	}
}
