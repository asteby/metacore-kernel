package dynamic

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"


	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/validate"
)

func reasonPolicyFor(p *manifest.ReasonRequiredDef) ReasonPolicyResolver {
	return func(_ context.Context, model string) (*manifest.ReasonRequiredDef, bool) {
		if model == "test_orders" && p != nil {
			return p, true
		}
		return nil, false
	}
}

func fieldCode(t *testing.T, err error, field string) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	fe := ve.Fields[field]
	if len(fe) == 0 {
		t.Fatalf("no field error on %q: %+v", field, ve.Fields)
	}
	return fe[0].Code
}

func TestDelete_ReasonRequired(t *testing.T) {
	fx, id := setupOrderFixture(t)
	bus := newFanOutBus()
	fx.svc.bus = bus
	fx.svc.reasonPolicies = reasonPolicyFor(&manifest.ReasonRequiredDef{Delete: true})

	// No reason -> 422 required, row still there.
	err := fx.svc.Delete(context.Background(), "test_orders", fx.user, id)
	if got := fieldCode(t, err, ReasonField); got != validate.CodeRequired {
		t.Fatalf("code = %q, want required", got)
	}
	// Too short -> min.
	err = fx.svc.Delete(WithReason(context.Background(), "ab"), "test_orders", fx.user, id)
	if got := fieldCode(t, err, ReasonField); got != validate.CodeMin {
		t.Fatalf("code = %q, want min", got)
	}
	if _, gerr := fx.svc.Get(context.Background(), "test_orders", fx.user, id); gerr != nil {
		t.Fatalf("row must survive a refused delete: %v", gerr)
	}
	// With a reason -> deleted, and the event carries it.
	if err := fx.svc.Delete(WithReason(context.Background(), "  duplicado por error  "), "test_orders", fx.user, id); err != nil {
		t.Fatalf("delete with reason: %v", err)
	}
	var found bool
	for _, p := range bus.published {
		if strings.HasSuffix(p.event, ".deleted") {
			ev, ok := p.payload.(*CanonicalEvent)
			if !ok || ev.Reason != "duplicado por error" {
				t.Fatalf("deleted event reason = %+v", p.payload)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no deleted event published")
	}
}

func TestDelete_NoPolicyNeedsNoReason(t *testing.T) {
	fx, id := setupOrderFixture(t)
	fx.svc.reasonPolicies = reasonPolicyFor(nil)
	if err := fx.svc.Delete(context.Background(), "test_orders", fx.user, id); err != nil {
		t.Fatalf("delete without policy: %v", err)
	}
}

func TestDelete_PolicyOnActionsOnlyDoesNotGateDelete(t *testing.T) {
	fx, id := setupOrderFixture(t)
	fx.svc.reasonPolicies = reasonPolicyFor(&manifest.ReasonRequiredDef{Actions: []string{"cancel"}})
	if err := fx.svc.Delete(context.Background(), "test_orders", fx.user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestDeleteHandler_ReadsReasonFromQueryAndBody(t *testing.T) {
	fx, id := setupOrderFixture(t)
	fx.svc.reasonPolicies = reasonPolicyFor(&manifest.ReasonRequiredDef{Delete: true, MinLength: 5})

	req := httptest.NewRequest("DELETE", "/dynamic/test_orders/"+id.String(), nil)
	resp, _ := fx.app.Test(req)
	if resp.StatusCode != 422 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("no reason: status %d body %s", resp.StatusCode, b)
	}
	req = httptest.NewRequest("DELETE", "/dynamic/test_orders/"+id.String()+"?reason=abc", nil)
	if resp, _ = fx.app.Test(req); resp.StatusCode != 422 {
		t.Fatalf("short reason: status %d", resp.StatusCode)
	}
	req = httptest.NewRequest("DELETE", "/dynamic/test_orders/"+id.String(), strings.NewReader(`{"reason":"captura duplicada"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, _ = fx.app.Test(req); resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("body reason: status %d body %s", resp.StatusCode, b)
	}
}

func TestExecAction_ReasonRequired(t *testing.T) {
	fx, id := setupOrderFixture(t)
	bus := newFanOutBus()
	fx.svc.bus = bus
	fx.svc.reasonPolicies = reasonPolicyFor(&manifest.ReasonRequiredDef{Actions: []string{"cancel"}})
	fx.registerAction("test_orders", &manifest.ActionDef{Key: "cancel", Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "cancel"}})
	fx.registerAction("test_orders", &manifest.ActionDef{Key: "advance", Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "advance"}})
	dispatched := 0
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		dispatched++
		return ActionResponse{Success: true}, nil
	}

	_, err := fx.svc.ExecAction(context.Background(), "test_orders", fx.user, id, "cancel", map[string]any{})
	if got := fieldCode(t, err, ReasonField); got != validate.CodeRequired {
		t.Fatalf("code = %q", got)
	}
	if dispatched != 0 {
		t.Fatal("a refused action must not dispatch")
	}
	// An action the policy does not list is untouched.
	if _, err := fx.svc.ExecAction(context.Background(), "test_orders", fx.user, id, "advance", map[string]any{}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	// With a reason: dispatched, and an audit event named after the action.
	if _, err := fx.svc.ExecAction(context.Background(), "test_orders", fx.user, id, "cancel", map[string]any{"reason": "el cliente se arrepintió"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var ev *CanonicalEvent
	for _, p := range bus.published {
		if strings.HasSuffix(p.event, ".cancel") {
			ev, _ = p.payload.(*CanonicalEvent)
		}
	}
	if ev == nil || ev.Reason != "el cliente se arrepintió" || ev.Action != "cancel" || ev.ID != id.String() {
		t.Fatalf("cancel event = %+v", ev)
	}
}

