package dynamic

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
)

// K3b: la ruta wasm (EnforceWasmStageWrite) aplica la misma politica que
// Service.Update. Reusa la maquina del fixture de P3:
//
//	draft -> confirmed (requires_fields reference) ; confirmed -> delivered (roles cashier, manager)
//	delivered -> confirmed (approval manager) ; confirmed -> cancelled ; delivered/cancelled locked
func wasmWrite(f *policyFixture, id uuid.UUID, op string, patch map[string]any) WasmStageWrite {
	before, err := f.svc.Get(context.Background(), "test_orders", f.user("manager"), id)
	if err != nil {
		panic(err)
	}
	sm, _ := f.svc.stageMachines(context.Background(), "test_orders")
	return WasmStageWrite{Model: "test_orders", Table: "test_orders", AddonKey: "sales", Op: op,
		RecordID: id.String(), OrgID: f.org, Machine: sm, Before: before, Patch: patch}
}

func asActor(u *fakeUser) context.Context {
	return WithActorID(context.Background(), u.GetID().String())
}

func enforcing(f *policyFixture) *policyFixture {
	f.svc.wasmStagePolicy = WasmStagePolicyEnforce
	return f
}

func TestWasmStagePolicy_VEN01_FinalStageIsLocked(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr := f.user("manager")
	id := f.order(t, mgr, "cancelled", "V1")
	// Una venta cancelada no admite ni editar otras columnas ni borrarse.
	err := f.svc.EnforceWasmStageWrite(asActor(mgr), wasmWrite(f, id, "update", map[string]any{"note": "x"}))
	if !isLocked(err) {
		t.Fatalf("edit of cancelled via wasm: %v, want record_locked", err)
	}
	err = f.svc.EnforceWasmStageWrite(asActor(mgr), wasmWrite(f, id, "delete", nil))
	if !isLocked(err) {
		t.Fatalf("delete of cancelled via wasm: %v, want record_locked", err)
	}
}

func TestWasmStagePolicy_VEN04_DeliveredReadOnlyExceptEditable(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr := f.user("manager")
	id := f.order(t, mgr, "delivered", "V4")
	ctx := asActor(mgr)
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, id, "update", map[string]any{"reference": "V4b"})); err != nil {
		t.Fatalf("editable_when_locked column rejected: %v", err)
	}
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, id, "update", map[string]any{"total": 9})); !isLocked(err) {
		t.Fatalf("locked column edit: %v", err)
	}
	// Un re-envio sin cambios no cuenta como edicion.
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, id, "update", map[string]any{"reference": "V4"})); err != nil {
		t.Fatalf("unchanged resend: %v", err)
	}
}

func TestWasmStagePolicy_TAL01_RolesAndRequiredFields(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr, agent := f.user("manager"), f.user("agent")
	draft := f.order(t, mgr, "draft", "")
	var ve *ValidationError
	if err := f.svc.EnforceWasmStageWrite(asActor(mgr), wasmWrite(f, draft, "update", map[string]any{"status": "confirmed"})); !errors.As(err, &ve) {
		t.Fatalf("confirm without reference: %v, want validation error", err)
	}
	if err := f.svc.EnforceWasmStageWrite(asActor(mgr), wasmWrite(f, draft, "update", map[string]any{"status": "confirmed", "reference": "T"})); err != nil {
		t.Fatal(err)
	}
	conf := f.order(t, mgr, "confirmed", "T2")
	if err := f.svc.EnforceWasmStageWrite(asActor(agent), wasmWrite(f, conf, "update", map[string]any{"status": "delivered"})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent delivering: %v, want forbidden", err)
	}
	if err := f.svc.EnforceWasmStageWrite(asActor(mgr), wasmWrite(f, conf, "update", map[string]any{"status": "delivered"})); err != nil {
		t.Fatalf("manager delivering: %v", err)
	}
}

// El sistema (sin actor) NO se salta roles: un handler sin usuario no entrega.
func TestWasmStagePolicy_SystemPrincipalFailsClosedOnRoles(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	id := f.order(t, f.user("manager"), "confirmed", "S1")
	w := wasmWrite(f, id, "update", map[string]any{"status": "delivered"})
	for name, ctx := range map[string]context.Context{
		"no actor":     context.Background(),
		"system id":    WithActorID(context.Background(), SystemActorID.String()),
		"garbage id":   WithActorID(context.Background(), "not-a-uuid"),
		"unknown user": WithActorID(context.Background(), uuid.NewString()),
	} {
		if err := f.svc.EnforceWasmStageWrite(ctx, w); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: %v, want forbidden", name, err)
		}
	}
}

// guard: la ruta wasm lo evalua sobre la fila fusionada, sin importar el actor.
func TestWasmStagePolicy_Guard(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr := f.user("manager")
	sm, _ := f.svc.stageMachines(context.Background(), "test_orders")
	sm.Transitions = append(sm.Transitions, manifest.TransitionDef{From: "draft", To: "cancelled", Guard: "paid >= total"})
	t.Cleanup(func() { sm.Transitions = sm.Transitions[:len(sm.Transitions)-1] })
	id := f.order(t, mgr, "draft", "G1")
	w := wasmWrite(f, id, "update", map[string]any{"status": "cancelled", "paid": 1, "total": 10})
	if err := f.svc.EnforceWasmStageWrite(asActor(mgr), w); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("guard false: %v", err)
	}
	w = wasmWrite(f, id, "update", map[string]any{"status": "cancelled", "paid": 10, "total": 10})
	if err := f.svc.EnforceWasmStageWrite(asActor(mgr), w); err != nil {
		t.Fatalf("guard true: %v", err)
	}
}

// Una transicion con approval se APARCA (misma ApprovalRequest y clave que la
// ruta REST), el handler recibe approval_required y, al aprobar, el replay corre
// como el solicitante y aplica UNA vez.
func TestWasmStagePolicy_ApprovalParkedAndReplayedOnce(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr, agent := f.user("manager"), f.user("agent")
	id := f.order(t, mgr, "delivered", "A1")
	w := wasmWrite(f, id, "update", map[string]any{"status": "confirmed"})

	err := f.svc.EnforceWasmStageWrite(asActor(agent), w)
	var wa *WasmStageApprovalError
	var ar *ApprovalRequiredError
	var ce *ConstraintError
	if !errors.As(err, &wa) || !errors.As(err, &ar) || !errors.As(err, &ce) {
		t.Fatalf("want parked approval, got %v", err)
	}
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatal("must unwrap to ErrApprovalRequired")
	}
	if ce.ErrorKey != "transition:delivered->confirmed" || !ce.Def.RequestsApproval() {
		t.Fatalf("policy key = %q", ce.ErrorKey)
	}
	req := ar.Request
	if req.Status != ApprovalStatusPending || req.ConstraintKey != "transition:delivered->confirmed" ||
		req.Kind != ApprovalKindConstraint || req.RequestedBy != agent.GetID() || req.RecordID != id.String() {
		t.Fatalf("request = %+v", req)
	}
	// Un reintento del mismo handler no apila solicitudes.
	err2 := f.svc.EnforceWasmStageWrite(asActor(agent), w)
	var ar2 *ApprovalRequiredError
	if !errors.As(err2, &ar2) || ar2.Request.ID != req.ID {
		t.Fatalf("retry must reuse request %s, got %v", req.ID, err2)
	}
	// Nada se aplico todavia.
	if got, _ := f.svc.Get(context.Background(), "test_orders", mgr, id); got["status"] != "delivered" {
		t.Fatalf("status moved before approval: %v", got["status"])
	}
	if _, err := f.svc.ApproveRequest(context.Background(), mgr, req.ID, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _ := f.svc.Get(context.Background(), "test_orders", mgr, id)
	if got["status"] != "confirmed" {
		t.Fatalf("status after approval = %v", got["status"])
	}
	// El replay no se puede aprobar dos veces (sin doble aplicacion).
	if _, err := f.svc.ApproveRequest(context.Background(), mgr, req.ID, ""); err == nil {
		t.Fatal("second approval must fail")
	}
}

// El sistema tampoco se salta approval: queda aparcado a nombre de SystemActorID.
func TestWasmStagePolicy_SystemDoesNotBypassApproval(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	id := f.order(t, f.user("manager"), "delivered", "A2")
	err := f.svc.EnforceWasmStageWrite(context.Background(), wasmWrite(f, id, "update", map[string]any{"status": "confirmed"}))
	var ar *ApprovalRequiredError
	if !errors.As(err, &ar) || ar.Request.RequestedBy != SystemActorID {
		t.Fatalf("system reopen: %v", err)
	}
}

// Action.stage_authority: la accion es la autoridad de ESA transicion (roles y
// approval), pero requires_fields/guard/locked siguen aplicando, y otra
// transicion u otro modelo no heredan nada.
func TestWasmStagePolicy_ActionStageAuthority(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr := f.user("manager")
	auth := &manifest.StageAuthorityDef{Transitions: []string{"delivered->confirmed", "draft->confirmed"}}
	ctx := WithActionStageAuthority(context.Background(), "test_orders", "reopen_sale", auth)

	deliv := f.order(t, mgr, "delivered", "AU1")
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, deliv, "update", map[string]any{"status": "confirmed"})); err != nil {
		t.Fatalf("authority must cover approval (system actor): %v", err)
	}
	// requires_fields sigue aplicando aunque la accion sea la autoridad.
	draft := f.order(t, mgr, "draft", "")
	var ve *ValidationError
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, draft, "update", map[string]any{"status": "confirmed"})); !errors.As(err, &ve) {
		t.Fatalf("requires_fields must still apply: %v", err)
	}
	// Una transicion NO declarada no hereda la autoridad (roles siguen).
	conf := f.order(t, mgr, "confirmed", "AU2")
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, conf, "update", map[string]any{"status": "delivered"})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("undeclared transition: %v", err)
	}
	// Otro modelo con el mismo nombre de transicion tampoco.
	other := WithActionStageAuthority(context.Background(), "invoices", "reopen_sale", auth)
	if err := f.svc.EnforceWasmStageWrite(other, wasmWrite(f, deliv, "update", map[string]any{"status": "confirmed"})); err == nil {
		t.Fatal("authority of another model must not apply")
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// Modo aviso (DEFAULT): no bloquea, registra stage_policy.wasm_bypass. off: nada.
func TestWasmStagePolicy_WarnIsDefaultAndOffIsSilent(t *testing.T) {
	f := setupPolicyFixture(t, false)
	if f.svc.WasmStagePolicy() != WasmStagePolicyWarn {
		t.Fatalf("default mode = %q, want warn", f.svc.WasmStagePolicy())
	}
	agent := f.user("agent")
	id := f.order(t, f.user("manager"), "delivered", "W1")
	w := wasmWrite(f, id, "update", map[string]any{"status": "confirmed"})
	buf := captureLog(t)
	if err := f.svc.EnforceWasmStageWrite(asActor(agent), w); err != nil {
		t.Fatalf("warn must not block: %v", err)
	}
	if !strings.Contains(buf.String(), "stage_policy.wasm_bypass") || !strings.Contains(buf.String(), "delivered->confirmed") {
		t.Fatalf("missing bypass log: %q", buf.String())
	}
	var n int64
	f.svc.db.Model(&ApprovalRequest{}).Count(&n)
	if n != 0 {
		t.Fatalf("warn must not park requests, found %d", n)
	}
	f.svc.wasmStagePolicy = WasmStagePolicyOff
	buf.Reset()
	if err := f.svc.EnforceWasmStageWrite(asActor(agent), w); err != nil || buf.Len() != 0 {
		t.Fatalf("off: err=%v log=%q", err, buf.String())
	}
	if WasmStagePolicyMode("ENFORCE ").normalize() != WasmStagePolicyEnforce || WasmStagePolicyMode("typo").normalize() != WasmStagePolicyWarn {
		t.Fatal("normalize")
	}
}

type ctxDispatcher struct {
	run func(ctx context.Context)
}

func (d ctxDispatcher) Dispatch(ctx context.Context, _ ActionRequest) (ActionResponse, error) {
	d.run(ctx)
	return ActionResponse{Success: true}, nil
}

// ExecAction deja la autoridad declarada del manifest en el contexto del
// handler wasm: una accion con stage_authority reabre sin aprobacion; la misma
// transicion desde una accion SIN el bloque queda aparcada.
func TestWasmStagePolicy_ExecActionCarriesStageAuthority(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr := f.user("manager")
	cashier := f.user("cashier")
	id := f.order(t, mgr, "delivered", "EX1")
	var got error
	f.svc.actionDispatchers = map[string]ActionDispatcher{"wasm": ctxDispatcher{run: func(ctx context.Context) {
		got = f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, id, "update", map[string]any{"status": "confirmed"}))
	}}}
	defs := map[string]*manifest.ActionDef{
		"reopen_sale": {Key: "reopen_sale", Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "x"},
			StageAuthority: &manifest.StageAuthorityDef{Transitions: []string{"delivered->confirmed"}}},
		"plain": {Key: "plain", Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "x"}},
	}
	f.svc.actionResolver = func(_ context.Context, _, key string) (*manifest.ActionDef, bool) {
		d, ok := defs[key]
		return d, ok
	}
	if _, err := f.svc.ExecAction(context.Background(), "test_orders", cashier, id, "reopen_sale", nil); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("action with stage_authority: %v", got)
	}
	if _, err := f.svc.ExecAction(context.Background(), "test_orders", cashier, id, "plain", nil); err != nil {
		t.Fatal(err)
	}
	var ar *ApprovalRequiredError
	if !errors.As(got, &ar) {
		t.Fatalf("action without stage_authority must park: %v", got)
	}
}

// El replay aprobado vale para UN registro: otro registro no hereda el bypass.
func TestWasmStagePolicy_ReplayMarkerScopedToRecord(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	mgr, agent := f.user("manager"), f.user("agent")
	a := f.order(t, mgr, "delivered", "RP1")
	b := f.order(t, mgr, "delivered", "RP2")
	ctx := WithApprovalReplay(asActor(agent), ApprovalReplay{
		Kind: ApprovalKindConstraint, ConstraintKey: transitionKey("delivered", "confirmed"),
		Model: "test_orders", RecordID: a.String(),
	})
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, a, "update", map[string]any{"status": "confirmed"})); err != nil {
		t.Fatalf("approved record: %v", err)
	}
	var ar *ApprovalRequiredError
	if err := f.svc.EnforceWasmStageWrite(ctx, wasmWrite(f, b, "update", map[string]any{"status": "confirmed"})); !errors.As(err, &ar) {
		t.Fatalf("other record must park, got %v", err)
	}
}

func TestWasmStagePolicy_WithoutStageAuthority(t *testing.T) {
	f := enforcing(setupPolicyFixture(t, false))
	auth := &manifest.StageAuthorityDef{Transitions: []string{"delivered->confirmed"}}
	ctx := WithActionStageAuthority(context.Background(), "test_orders", "k", auth)
	if !HasStageAuthority(ctx) || HasStageAuthority(WithoutStageAuthority(ctx)) {
		t.Fatal("WithoutStageAuthority must drop the authority")
	}
	id := f.order(t, f.user("manager"), "delivered", "WS1")
	var ar *ApprovalRequiredError
	if err := f.svc.EnforceWasmStageWrite(WithoutStageAuthority(ctx), wasmWrite(f, id, "update", map[string]any{"status": "confirmed"})); !errors.As(err, &ar) {
		t.Fatalf("stripped ctx must park: %v", err)
	}
}
