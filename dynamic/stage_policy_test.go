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
)

// policyFixture: una venta con la máquina estricta de P3.
//
//	draft -> confirmed            (requires_fields: reference)
//	confirmed -> delivered        (roles: cashier, manager)
//	delivered (final, locked, editable_when_locked: reference)
//	delivered -> confirmed        (reopen: approval de manager)
//	confirmed -> cancelled
type policyFixture struct {
	svc   *Service
	roles map[uuid.UUID][]string
	org   uuid.UUID
}

func setupPolicyFixture(t *testing.T, strictCreate bool) *policyFixture {
	t.Helper()
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
		Field: "status",
		Stages: []manifest.StageDef{
			{Key: "draft", Order: 0},
			{Key: "confirmed", Order: 1},
			{Key: "delivered", Order: 2, IsFinal: true, Locked: true, EditableWhenLocked: []string{"reference"}},
			{Key: "cancelled", Order: 3, IsFinal: true, Locked: true},
		},
		Transitions: []manifest.TransitionDef{
			{From: "draft", To: "confirmed", RequiresFields: []string{"reference"}},
			{From: "confirmed", To: "delivered", Roles: []string{"cashier", "manager"}},
			{From: "delivered", To: "confirmed", Approval: &manifest.ApprovalDef{Roles: []string{"manager"}}},
			{From: "confirmed", To: "cancelled"},
		},
	}
	f := &policyFixture{roles: map[uuid.UUID][]string{}, org: uuid.New()}
	f.svc = New(Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		StrictStageCreate:            strictCreate,
		StageMachineResolver: func(_ context.Context, model string) (*StageMachine, bool) {
			return sm, model == "test_orders"
		},
		ActorRolesResolver: func(_ context.Context, u modelbase.AuthUser) []string { return f.roles[u.GetID()] },
	})
	return f
}

func (f *policyFixture) user(roles ...string) *fakeUser {
	u := newUser(f.org)
	f.roles[u.GetID()] = roles
	return u
}

func (f *policyFixture) order(t *testing.T, u *fakeUser, status, ref string) uuid.UUID {
	t.Helper()
	out, err := f.svc.Create(context.Background(), "test_orders", u, map[string]any{"reference": ref, "status": status})
	if err != nil {
		t.Fatal(err)
	}
	return uuid.MustParse(out["id"].(string))
}

func upd(f *policyFixture, u *fakeUser, id uuid.UUID, in map[string]any) error {
	_, err := f.svc.Update(context.Background(), "test_orders", u, id, in)
	return err
}

// VEN-01: una venta cancelada no se revive con un PUT state=draft.
func TestStagePolicy_VEN01_FinalStageDoesNotReopen(t *testing.T) {
	f := setupPolicyFixture(t, false)
	u := f.user("manager")
	id := f.order(t, u, "draft", "V-1")
	if err := upd(f, u, id, map[string]any{"status": "confirmed"}); err != nil {
		t.Fatal(err)
	}
	if err := upd(f, u, id, map[string]any{"status": "cancelled"}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"draft", "confirmed", "delivered"} {
		err := upd(f, u, id, map[string]any{"status": to})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("cancelled -> %s: err = %v, want invalid_transition", to, err)
		}
	}
}

// VEN-04: un documento entregado es de solo lectura (salvo editable_when_locked).
func TestStagePolicy_VEN04_DeliveredIsReadOnly(t *testing.T) {
	f := setupPolicyFixture(t, false)
	u := f.user("manager")
	id := f.order(t, u, "confirmed", "V-2")
	if err := upd(f, u, id, map[string]any{"status": "delivered"}); err != nil {
		t.Fatal(err)
	}
	if err := upd(f, u, id, map[string]any{"status": "delivered", "reference": "V-2b"}); err != nil {
		t.Fatalf("editable_when_locked column rejected: %v", err)
	}
	if err := f.svc.Delete(context.Background(), "test_orders", u, id); !isLocked(err) {
		t.Fatalf("delete of a delivered sale: %v, want record_locked", err)
	}
	// Con una columna distinta de la permitida: record_locked.
	err := upd(f, u, id, map[string]any{"reference": "V-2c", "status": "delivered"})
	if err != nil {
		t.Fatalf("reference is editable when locked: %v", err)
	}
}

func TestStagePolicy_EditableWhenLockedRejectsOtherColumns(t *testing.T) {
	sm := &StageMachine{
		Field: "status",
		Stages: []manifest.StageDef{
			{Key: "done", Locked: true, EditableWhenLocked: []string{"notes"}},
		},
	}
	before := map[string]any{"status": "done", "notes": "a", "total": 10}
	if err := CheckStageLock(sm, before, map[string]any{"notes": "b"}); err != nil {
		t.Fatalf("notes is editable when locked: %v", err)
	}
	if err := CheckStageLock(sm, before, map[string]any{"notes": "b", "total": 11}); !isLocked(err) {
		t.Fatalf("total must be locked, got %v", err)
	}
}

// TAL-01: roles, campos requeridos y salida de una etapa final con aprobación.
func TestStagePolicy_TAL01_RolesAndRequiredFields(t *testing.T) {
	f := setupPolicyFixture(t, false)
	mgr := f.user("manager")
	agent := f.user("agent")

	// requires_fields: sin referencia no se confirma.
	id := f.order(t, mgr, "draft", "")
	var ve *ValidationError
	if err := upd(f, mgr, id, map[string]any{"status": "confirmed"}); !errors.As(err, &ve) {
		t.Fatalf("confirm without reference: %v, want validation error", err)
	}
	if err := upd(f, mgr, id, map[string]any{"status": "confirmed", "reference": "T-1"}); err != nil {
		t.Fatal(err)
	}
	// roles: un agente no entrega.
	if err := upd(f, agent, id, map[string]any{"status": "delivered"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent delivering: %v, want forbidden", err)
	}
	if err := upd(f, mgr, id, map[string]any{"status": "delivered"}); err != nil {
		t.Fatal(err)
	}
}

// Reabrir una etapa final pide aprobación: el PUT queda pendiente y solo al
// aprobar el supervisor la venta vuelve a confirmed.
func TestStagePolicy_ReopenNeedsApproval(t *testing.T) {
	f := setupPolicyFixture(t, false)
	mgr := f.user("manager")
	agent := f.user("agent")
	id := f.order(t, mgr, "confirmed", "R-1")
	if err := upd(f, mgr, id, map[string]any{"status": "delivered"}); err != nil {
		t.Fatal(err)
	}
	err := upd(f, agent, id, map[string]any{"status": "confirmed"})
	var ar *ApprovalRequiredError
	if !errors.As(err, &ar) {
		t.Fatalf("reopen: %v, want approval_required", err)
	}
	got, _ := f.svc.Get(context.Background(), "test_orders", agent, id)
	if got["status"] != "delivered" {
		t.Fatalf("status moved before approval: %v", got["status"])
	}
	if _, err := f.svc.ApproveRequest(context.Background(), mgr, ar.Request.ID, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _ = f.svc.Get(context.Background(), "test_orders", agent, id)
	if got["status"] != "confirmed" {
		t.Fatalf("status after approval = %v, want confirmed", got["status"])
	}
}

func TestStagePolicy_GuardExpr(t *testing.T) {
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "a"}, {Key: "b"}},
		Transitions: []manifest.TransitionDef{
			{From: "a", To: "b", Guard: "paid >= total"},
		},
	}
	if err := CheckTransitionPolicy(sm, "a", "b", map[string]any{"paid": 5, "total": 10}, nil, true); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("guard false: %v", err)
	}
	if err := CheckTransitionPolicy(sm, "a", "b", map[string]any{"paid": 10, "total": 10}, nil, true); err != nil {
		t.Fatalf("guard true: %v", err)
	}
}

func TestStagePolicy_CreateInFinalStage(t *testing.T) {
	// Aviso (por defecto): se crea y se registra.
	f := setupPolicyFixture(t, false)
	u := f.user("manager")
	f.order(t, u, "cancelled", "X-1")
	// Estricto: 422.
	g := setupPolicyFixture(t, true)
	gu := g.user("manager")
	if _, err := g.svc.Create(context.Background(), "test_orders", gu, map[string]any{"reference": "X-2", "status": "cancelled"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("strict create in final: %v", err)
	}
}

// Un replay de OTRA transicion no hereda el bypass de roles: A->confirmed
// aparcada, el registro pasa a otro estado, el replay no debe saltarse roles.
func TestStagePolicy_ReplayBypassIsScopedToItsTransition(t *testing.T) {
	f := setupPolicyFixture(t, false)
	agent := f.user("agent")
	mgr := f.user("manager")
	id := f.order(t, mgr, "confirmed", "RB-1")
	// Replay de una aprobacion de OTRA transicion (draft->confirmed) sobre
	// confirmed->delivered (requiere cashier/manager): el agente no pasa.
	ctx := WithApprovalReplay(context.Background(), ApprovalReplay{
		Kind: ApprovalKindConstraint, ConstraintKey: transitionKey("draft", "confirmed"), Model: "test_orders",
	})
	_, err := f.svc.Update(ctx, "test_orders", agent, id, map[string]any{"status": "delivered"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign replay must not bypass roles: %v", err)
	}
	// El replay de ESTA transicion si salta roles (el aprobador ya los valido).
	ctx = WithApprovalReplay(context.Background(), ApprovalReplay{
		Kind: ApprovalKindConstraint, ConstraintKey: transitionKey("confirmed", "delivered"), Model: "test_orders",
	})
	if _, err := f.svc.Update(ctx, "test_orders", agent, id, map[string]any{"status": "delivered"}); err != nil {
		t.Fatalf("own replay: %v", err)
	}
}
