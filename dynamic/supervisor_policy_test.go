package dynamic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

func grantFor(t *testing.T, svc *Service, cashier *fakeUser, policy, record string) string {
	t.Helper()
	req, err := svc.GrantPINApproval(context.Background(), cashier, PINGrantInput{
		PolicyKey: policy, Reason: "cliente esperando", PIN: "4321", RecordID: record,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return req.ID.String()
}

func TestConsumePINGrant(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	id := grantFor(t, svc, cashier, "cancel_cfdi", "")

	// Wrong policy, unknown id, garbage id: refused and NOT consumed.
	for _, c := range []struct{ id, policy string }{
		{id, "refund"}, {uuid.NewString(), "cancel_cfdi"}, {"nope", "cancel_cfdi"}, {"", "cancel_cfdi"},
	} {
		if err := svc.ConsumePINGrant(ctx, cashier, c.id, c.policy, ""); !errors.Is(err, ErrApprovalGrantRequired) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	// Another user cannot spend the cashier's grant.
	other := &fakeUser{id: uuid.New(), orgID: cashier.orgID, role: "cashier"}
	if err := svc.ConsumePINGrant(ctx, other, id, "cancel_cfdi", ""); !errors.Is(err, ErrApprovalGrantRequired) {
		t.Fatalf("stolen grant: %v", err)
	}
	// A foreign org cannot see it either.
	foreign := &fakeUser{id: cashier.id, orgID: uuid.New(), role: "cashier"}
	if err := svc.ConsumePINGrant(ctx, foreign, id, "cancel_cfdi", ""); !errors.Is(err, ErrApprovalGrantRequired) {
		t.Fatalf("cross-org: %v", err)
	}
	// The right caller redeems it once, and only once.
	if err := svc.ConsumePINGrant(ctx, cashier, id, "cancel_cfdi", ""); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := svc.ConsumePINGrant(ctx, cashier, id, "cancel_cfdi", ""); !errors.Is(err, ErrApprovalGrantRequired) {
		t.Fatalf("second use must fail: %v", err)
	}
}

func TestConsumePINGrant_ExpiryAndRecordAnchor(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()

	anchored := grantFor(t, svc, cashier, "refund", "rec-1")
	if err := svc.ConsumePINGrant(ctx, cashier, anchored, "refund", "rec-2"); !errors.Is(err, ErrApprovalGrantRequired) {
		t.Fatalf("grant for another record: %v", err)
	}
	if err := svc.ConsumePINGrant(ctx, cashier, anchored, "refund", "rec-1"); err != nil {
		t.Fatalf("anchored grant on its record: %v", err)
	}

	stale := grantFor(t, svc, cashier, "refund", "")
	old := time.Now().Add(-PINGrantMaxAge - time.Minute)
	if err := svc.db.Model(&ApprovalRequest{}).Where("id = ?", stale).Update("decided_at", old).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.ConsumePINGrant(ctx, cashier, stale, "refund", ""); !errors.Is(err, ErrApprovalGrantRequired) {
		t.Fatalf("stale grant: %v", err)
	}
}

func TestExecAction_SupervisorPolicy(t *testing.T) {
	fx, id := setupOrderFixture(t)
	manager := &fakeUser{id: uuid.New(), orgID: fx.user.orgID, role: "manager"}
	fx.svc.approvalPINVerifier = func(_ context.Context, _ uuid.UUID, _, pin string) (modelbase.AuthUser, error) {
		if pin == "4321" {
			return manager, nil
		}
		return nil, errors.New("no match")
	}
	fx.registerAction("test_orders", &manifest.ActionDef{
		Key: "cancel_cfdi", Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "x"}, SupervisorPolicy: "cancel_cfdi",
	})
	dispatched := 0
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		dispatched++
		return ActionResponse{Success: true}, nil
	}
	ctx := context.Background()

	// No grant, no bypass: refused, nothing dispatched.
	if _, err := fx.svc.ExecAction(ctx, "test_orders", fx.user, id, "cancel_cfdi", map[string]any{}); !errors.Is(err, ErrApprovalGrantRequired) {
		t.Fatalf("no grant: %v", err)
	}
	if dispatched != 0 {
		t.Fatal("refused action dispatched")
	}
	// With a grant: dispatched, and the grant cannot be replayed.
	g := grantFor(t, fx.svc, fx.user, "cancel_cfdi", "")
	if _, err := fx.svc.ExecAction(ctx, "test_orders", fx.user, id, "cancel_cfdi", map[string]any{"approval_id": g}); err != nil {
		t.Fatalf("with grant: %v", err)
	}
	if _, err := fx.svc.ExecAction(ctx, "test_orders", fx.user, id, "cancel_cfdi", map[string]any{"approval_id": g}); !errors.Is(err, ErrApprovalGrantRequired) {
		t.Fatalf("replayed grant: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1", dispatched)
	}
	// A holder of the capability (bypass) needs no grant.
	fx.svc.supervisorBypass = func(_ context.Context, _ modelbase.AuthUser, policy string) bool { return policy == "cancel_cfdi" }
	if _, err := fx.svc.ExecAction(ctx, "test_orders", fx.user, id, "cancel_cfdi", map[string]any{}); err != nil {
		t.Fatalf("bypass: %v", err)
	}
}
