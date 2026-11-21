package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
)

// transitions[].permission delegates ONE move to a capability without a role:
// holding the capability is enough, the roles gate still works for others, and
// requires_fields keeps applying.
func TestTransitionPermission_StandsInForRoles(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS test_orders (
		id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
		reference TEXT, status TEXT)`)
	modelbase.Register("test_orders", func() modelbase.ModelDefiner { return &TestOrder{} })
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "draft"}, {Key: "confirmed"}},
		Transitions: []manifest.TransitionDef{
			{From: "draft", To: "confirmed", Roles: []string{"cashier"}, Permission: "sales.confirm", RequiresFields: []string{"reference"}},
		},
	}
	org := uuid.New()
	delegated := newUser(org) // role agent, holds the capability
	plain := newUser(org)     // role agent, nothing extra
	cashier := newUser(org)   // role cashier, no capability
	crud := []permission.Capability{"test_orders.read", "test_orders.create", "test_orders.update"}
	store := permission.NewInMemoryStore(nil, map[uuid.UUID][]permission.Capability{
		delegated.GetID(): append([]permission.Capability{"sales.confirm"}, crud...),
		plain.GetID():     crud,
		cashier.GetID():   crud,
	})
	roles := map[uuid.UUID][]string{delegated.GetID(): {"agent"}, plain.GetID(): {"agent"}, cashier.GetID(): {"cashier"}}
	svc := New(Config{
		DB:          db,
		Metadata:    metadata.New(metadata.Config{CacheTTL: -1}),
		Permissions: permission.New(permission.Config{Store: store, CacheTTL: -1, SuperRoles: []permission.Role{}}),
		StageMachineResolver: func(_ context.Context, model string) (*StageMachine, bool) {
			return sm, model == "test_orders"
		},
		ActorRolesResolver: func(_ context.Context, u modelbase.AuthUser) []string { return roles[u.GetID()] },
	})
	mk := func(u *fakeUser, ref string) uuid.UUID {
		out, err := svc.Create(context.Background(), "test_orders", u, map[string]any{"reference": ref, "status": "draft"})
		if err != nil {
			t.Fatal(err)
		}
		return uuid.MustParse(out["id"].(string))
	}
	move := func(u *fakeUser, id uuid.UUID) error {
		_, err := svc.Update(context.Background(), "test_orders", u, id, map[string]any{"status": "confirmed"})
		return err
	}
	if err := move(plain, mk(plain, "R-1")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no role, no capability: %v want forbidden", err)
	}
	if err := move(delegated, mk(delegated, "R-2")); err != nil {
		t.Fatalf("capability holder must make the move: %v", err)
	}
	if err := move(cashier, mk(cashier, "R-3")); err != nil {
		t.Fatalf("the role path must keep working: %v", err)
	}
	var ve *ValidationError
	if err := move(delegated, mk(delegated, "")); !errors.As(err, &ve) {
		t.Fatalf("requires_fields still applies to a delegated move: %v", err)
	}
}
