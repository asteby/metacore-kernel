package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// pinFixture: one org, a cashier, a manager who owns PIN "4321" and a service
// whose verifier only accepts that PIN for the manager.
func pinFixture(t *testing.T) (*Service, *fakeUser, *fakeUser) {
	t.Helper()
	db := setupTestDB(t)
	org := uuid.New()
	cashier := &fakeUser{id: uuid.New(), orgID: org, role: "cashier"}
	manager := &fakeUser{id: uuid.New(), orgID: org, role: "manager"}
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ApprovalPINVerifier: func(_ context.Context, o uuid.UUID, policy, pin string) (modelbase.AuthUser, error) {
			if o == org && pin == "4321" {
				return manager, nil
			}
			return nil, errors.New("no match")
		},
	})
	return svc, cashier, manager
}

func TestGrantPINApproval(t *testing.T) {
	svc, cashier, manager := pinFixture(t)
	ctx := context.Background()

	req, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{
		PolicyKey: "pos.oversell", Label: "Venta sin stock", Reason: "cliente esperando", PIN: "4321",
		Context: map[string]any{"product": "X", "qty": 3},
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if req.Kind != ApprovalKindPIN || req.Status != ApprovalStatusApplied {
		t.Fatalf("kind/status = %s/%s", req.Kind, req.Status)
	}
	if req.RequestedBy != cashier.id || req.DecidedBy == nil || *req.DecidedBy != manager.id {
		t.Fatalf("requester/approver not recorded: %+v", req)
	}
	if req.Reason != "cliente esperando" || req.ConstraintKey != "pos.oversell" {
		t.Fatalf("audit fields: %+v", req)
	}
	got, err := svc.GetApproval(ctx, cashier, req.ID)
	if err != nil || got.Status != ApprovalStatusApplied {
		t.Fatalf("persisted row: %v %+v", err, got)
	}
}

func TestGrantPINApprovalRejections(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()

	if _, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", PIN: "4321"}); !errors.Is(err, ErrApprovalReasonRequired) {
		t.Fatalf("missing reason: %v", err)
	}
	if _, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PIN: "4321", Reason: "r"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing policy: %v", err)
	}
	if _, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "0000"}); !errors.Is(err, ErrApprovalPINInvalid) {
		t.Fatalf("wrong pin: %v", err)
	}
	// Foreign-org approver is refused even with a matching PIN.
	foreign := &fakeUser{id: uuid.New(), orgID: uuid.New(), role: "manager"}
	svc.approvalPINVerifier = func(context.Context, uuid.UUID, string, string) (modelbase.AuthUser, error) { return foreign, nil }
	if _, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321"}); !errors.Is(err, ErrApprovalPINInvalid) {
		t.Fatalf("foreign approver: %v", err)
	}
	// No verifier wired.
	bare := New(Config{DB: setupTestDB(t), Metadata: metadata.New(metadata.Config{CacheTTL: -1})})
	if _, err := bare.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "1"}); !errors.Is(err, ErrApprovalPINUnavailable) {
		t.Fatalf("unwired: %v", err)
	}
}

func TestPINThrottleLocksAfterFailures(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	for i := 0; i < pinMaxFailures; i++ {
		if _, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "bad"}); !errors.Is(err, ErrApprovalPINInvalid) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Even the right PIN is refused while locked.
	if _, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321"}); !errors.Is(err, ErrApprovalPINLocked) {
		t.Fatalf("expected lock, got %v", err)
	}
}

func TestApproveRequestWithPIN(t *testing.T) {
	svc, cashier, manager := pinFixture(t)
	ctx := context.Background()
	svc.actorRolesResolver = func(_ context.Context, u modelbase.AuthUser) []string { return []string{u.GetRole()} }

	parked, err := svc.RequestApproval(ctx, ApprovalInput{
		OrgID: cashier.orgID, ModelKey: "TestProduct", Label: "x", RequestedBy: cashier.id,
		Roles: []string{"manager"}, ReasonRequired: true,
		Payload: map[string]any{"op": "noop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.RegisterApprovalApplier("noop", func(context.Context, *Service, *ApprovalRequest, modelbase.AuthUser) (any, error) {
		return map[string]any{"ok": true}, nil
	})

	if _, err := svc.ApproveRequestWithPIN(ctx, cashier, parked.ID, "4321", ""); !errors.Is(err, ErrApprovalReasonRequired) {
		t.Fatalf("reason gate: %v", err)
	}
	done, err := svc.ApproveRequestWithPIN(ctx, cashier, parked.ID, "4321", "ok, lo autorizo")
	if err != nil {
		t.Fatalf("approve-pin: %v", err)
	}
	if done.Status != ApprovalStatusApplied || done.DecidedBy == nil || *done.DecidedBy != manager.id {
		t.Fatalf("decision: %+v", done)
	}
}

func TestApprovalPINRoutes(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	app := fiber.New()
	NewHandler(svc, fakeUserResolver(cashier)).MountApprovals(app.Group("/api"))

	post := func(path, body string) (int, map[string]any) {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	if code, out := post("/api/approvals/pin-grant", `{"policy":"pos.oversell","pin":"nope","reason":"r"}`); code != 403 || out["error"].(map[string]any)["code"] != "approval_pin_invalid" {
		t.Fatalf("bad pin: %d %v", code, out)
	}
	if code, out := post("/api/approvals/pin-grant", `{"policy":"pos.oversell","pin":"4321","reason":"r"}`); code != 201 || out["success"] != true {
		t.Fatalf("grant: %d %v", code, out)
	}
	if code, out := post("/api/approvals/pin-grant", `{"policy":"pos.oversell","pin":"4321"}`); code != 422 || out["error"].(map[string]any)["code"] != "approval_reason_required" {
		t.Fatalf("no reason: %d %v", code, out)
	}
}

func TestConsumeApproval(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	grant := func(policy string, c map[string]any) uuid.UUID {
		t.Helper()
		req, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: policy, Reason: "r", PIN: "4321", Context: c})
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		return req.ID
	}
	in := func(id uuid.UUID) ConsumeInput {
		return ConsumeInput{
			OrgID: cashier.orgID, ApprovalID: id, Policy: "price_below_min", RequestedBy: cashier.id,
			ContextEq:  map[string]string{"product_id": "p1"},
			ContextGte: map[string]float64{"unit_shortfall": 5},
		}
	}

	id := grant("price_below_min", map[string]any{"product_id": "p1", "unit_shortfall": 7.5})
	if err := svc.ConsumeApproval(ctx, in(id)); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := svc.ConsumeApproval(ctx, in(id)); !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("second use must fail: %v", err)
	}

	cases := map[string]func(ConsumeInput) ConsumeInput{
		"otra org":         func(c ConsumeInput) ConsumeInput { c.OrgID = uuid.New(); return c },
		"otra politica":    func(c ConsumeInput) ConsumeInput { c.Policy = "discount"; return c },
		"otro solicitante": func(c ConsumeInput) ConsumeInput { c.RequestedBy = uuid.New(); return c },
		"otro producto":    func(c ConsumeInput) ConsumeInput { c.ContextEq = map[string]string{"product_id": "p2"}; return c },
		"no cubre":         func(c ConsumeInput) ConsumeInput { c.ContextGte = map[string]float64{"unit_shortfall": 9}; return c },
		"id inexistente":   func(c ConsumeInput) ConsumeInput { c.ApprovalID = uuid.New(); return c },
	}
	for name, mod := range cases {
		fresh := grant("price_below_min", map[string]any{"product_id": "p1", "unit_shortfall": 7.5})
		if err := svc.ConsumeApproval(ctx, mod(in(fresh))); !errors.Is(err, ErrApprovalConsume) {
			t.Errorf("%s: %v", name, err)
		}
		// A refused attempt must not burn the grant.
		if name != "id inexistente" {
			if err := svc.ConsumeApproval(ctx, in(fresh)); err != nil {
				t.Errorf("%s: el rechazo quemó la autorización: %v", name, err)
			}
		}
	}
}

func TestConsumeApprovalNonFiniteAmountsCoverNothing(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	for _, bad := range []any{"Infinity", "+Inf", "NaN", "inf"} {
		req, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "price_below_min", Reason: "r", PIN: "4321",
			Context: map[string]any{"product_id": "p1", "unit_shortfall": bad}})
		if err != nil {
			t.Fatal(err)
		}
		err = svc.ConsumeApproval(ctx, ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "price_below_min",
			RequestedBy: cashier.id, ContextGte: map[string]float64{"unit_shortfall": 5}})
		if !errors.Is(err, ErrApprovalConsume) {
			t.Errorf("%v no debe cubrir un faltante: %v", bad, err)
		}
	}
	// A non-finite REQUIRED amount is refused too.
	req, _ := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "price_below_min", Reason: "r", PIN: "4321",
		Context: map[string]any{"unit_shortfall": 9.0}})
	err := svc.ConsumeApproval(ctx, ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "price_below_min",
		RequestedBy: cashier.id, ContextGte: map[string]float64{"unit_shortfall": math.Inf(1)}})
	if !errors.Is(err, ErrApprovalConsume) {
		t.Errorf("monto requerido infinito: %v", err)
	}
}

// The state is checked INSIDE the conditional UPDATE: a grant revoked between
// the read and the redemption cannot be consumed.
func TestConsumeApprovalRechecksStateAtomically(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	req, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321", Context: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	svc.db.Callback().Query().After("gorm:query").Register("test:revoke_after_read", func(tx *gorm.DB) {
		if fired || tx.Statement.Table != "approval_requests" {
			return
		}
		fired = true
		svc.db.Session(&gorm.Session{NewDB: true}).Exec("UPDATE approval_requests SET status = 'rejected' WHERE id = ?", req.ID)
	})
	err = svc.ConsumeApproval(ctx, ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "p", RequestedBy: cashier.id})
	if !fired {
		t.Skip("el callback no se disparó en este driver")
	}
	if !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("una autorización revocada tras la lectura no debe canjearse: %v", err)
	}
}

func TestConsumeApprovalContextEqNumeric(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	req, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321",
		Context: map[string]any{"qty": 1000000.0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ConsumeApproval(ctx, ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "p",
		RequestedBy: cashier.id, ContextEq: map[string]string{"qty": "1000000"}}); err != nil {
		t.Fatalf("1e6 debe empatar con \"1000000\": %v", err)
	}
}

func TestConsumeApprovalRequiresContext(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	req, _ := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321"})
	err := svc.ConsumeApproval(ctx, ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "p", RequestedBy: cashier.id})
	if !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("contexto vacío debe rechazarse: %v", err)
	}
}

// A grant anchored to addon / model / record only redeems for the same ones.
func TestConsumeApprovalAnchors(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	req, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321",
		AddonKey: "pos", ModelKey: "sale_items", RecordID: "r1", Context: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	base := ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "p", RequestedBy: cashier.id,
		ContextEq: map[string]string{"k": "v"}, AddonKey: "pos", ModelKey: "sale_items", RecordID: "r1"}
	for name, mod := range map[string]func(*ConsumeInput){
		"otro addon":    func(c *ConsumeInput) { c.AddonKey = "other" },
		"sin addon":     func(c *ConsumeInput) { c.AddonKey = "" },
		"otro modelo":   func(c *ConsumeInput) { c.ModelKey = "orders" },
		"otro registro": func(c *ConsumeInput) { c.RecordID = "r2" },
		"sin registro":  func(c *ConsumeInput) { c.RecordID = "" },
	} {
		c := base
		mod(&c)
		if err := svc.ConsumeApproval(ctx, c); !errors.Is(err, ErrApprovalConsume) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := svc.ConsumeApproval(ctx, base); err != nil {
		t.Fatalf("los rechazos no queman y el ancla correcta canjea: %v", err)
	}
}

// A grant of someone else in the same org is indistinguishable from a missing one,
// whatever its state (wrong policy, used, ...).
func TestConsumeApprovalNoOracleForOthersGrants(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	req, _ := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321", Context: map[string]any{"k": "v"}})
	other := uuid.New()
	mk := func(id uuid.UUID, policy string) ConsumeInput {
		return ConsumeInput{OrgID: cashier.orgID, ApprovalID: id, Policy: policy, RequestedBy: other, ContextEq: map[string]string{"k": "v"}}
	}
	missing := svc.ConsumeApproval(ctx, mk(uuid.New(), "p"))
	for _, policy := range []string{"p", "wrong"} {
		got := svc.ConsumeApproval(ctx, mk(req.ID, policy))
		if got == nil || missing == nil || got.Error() != missing.Error() {
			t.Fatalf("policy=%s: %v != %v", policy, got, missing)
		}
	}
}

func TestConsumeApprovalAbsentContextKeyCoversNothing(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	req, _ := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321", Context: map[string]any{"other": 9.0}})
	err := svc.ConsumeApproval(ctx, ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "p", RequestedBy: cashier.id,
		ContextGte: map[string]float64{"unit_shortfall": 0}})
	if !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("clave ausente no es 0: %v", err)
	}
}

// A database failure is an internal error, not "unknown grant".
func TestConsumeApprovalDBErrorIsNotARefusal(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	if err := svc.ensureApprovalTable(); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.Exec("DROP TABLE approval_requests").Error; err != nil {
		t.Fatal(err)
	}
	err := svc.ConsumeApproval(context.Background(), ConsumeInput{OrgID: cashier.orgID, ApprovalID: uuid.New(), Policy: "p",
		RequestedBy: cashier.id, ContextEq: map[string]string{"k": "v"}})
	if err == nil || errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("un fallo de DB no es un rechazo: %v", err)
	}
}

// Redeeming on the caller's transaction: a rollback leaves the grant unspent.
func TestConsumeApprovalRollsBackWithTx(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	ctx := context.Background()
	req, _ := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "p", Reason: "r", PIN: "4321", Context: map[string]any{"k": "v"}})
	in := ConsumeInput{OrgID: cashier.orgID, ApprovalID: req.ID, Policy: "p", RequestedBy: cashier.id, ContextEq: map[string]string{"k": "v"}}
	tx := svc.db.Begin()
	in.DB = tx
	if err := svc.ConsumeApproval(ctx, in); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	in.DB = nil
	if err := svc.ConsumeApproval(ctx, in); err != nil {
		t.Fatalf("tras rollback la autorización debe seguir vigente: %v", err)
	}
}
