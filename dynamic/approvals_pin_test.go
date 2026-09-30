package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

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
