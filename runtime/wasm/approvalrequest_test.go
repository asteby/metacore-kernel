package wasm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

type arEnvelope struct {
	Success bool           `json:"success"`
	Data    *arData        `json:"data,omitempty"`
	Error   *dbqError      `json:"error,omitempty"`
	Meta    map[string]any `json:"meta"`
}

type arData struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func unmarshalApprovalRequest(t *testing.T, raw []byte) arEnvelope {
	t.Helper()
	var env arEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v -- %s", err, raw)
	}
	return env
}

func TestExecuteApprovalRequest_NoRequesterConfigured(t *testing.T) {
	inv := &invocation{addonKey: "pricing", orgID: uuid.New()}
	env := unmarshalApprovalRequest(t, executeApprovalRequest(context.Background(), inv, []byte(`{
		"roles": ["manager"],
		"payload": {"op": "wasm_callback"}
	}`)))
	if env.Success {
		t.Fatalf("expected failure, got success")
	}
	if env.Error.Code != "approvals_unavailable" {
		t.Fatalf("expected approvals_unavailable, got %q", env.Error.Code)
	}
}

func TestExecuteApprovalRequest_ValidatesRolesAndOp(t *testing.T) {
	inv := &invocation{
		addonKey: "pricing",
		orgID:    uuid.New(),
		approvalRequester: func(ctx context.Context, in dynamic.ApprovalInput) (*dynamic.ApprovalRequest, error) {
			t.Fatalf("requester should not be called for an invalid request")
			return nil, nil
		},
	}

	env := unmarshalApprovalRequest(t, executeApprovalRequest(context.Background(), inv, []byte(`{
		"payload": {"op": "wasm_callback"}
	}`)))
	if env.Success || env.Error.Code != "invalid_request" {
		t.Fatalf("expected invalid_request for missing roles, got %+v", env)
	}

	env = unmarshalApprovalRequest(t, executeApprovalRequest(context.Background(), inv, []byte(`{
		"roles": ["manager"],
		"payload": {}
	}`)))
	if env.Success || env.Error.Code != "invalid_request" {
		t.Fatalf("expected invalid_request for missing payload.op, got %+v", env)
	}
}

func TestExecuteApprovalRequest_Success(t *testing.T) {
	orgID := uuid.New()
	created := &dynamic.ApprovalRequest{ID: uuid.New(), Status: dynamic.ApprovalStatusPending}
	var captured dynamic.ApprovalInput
	inv := &invocation{
		addonKey: "pricing",
		orgID:    orgID,
		approvalRequester: func(ctx context.Context, in dynamic.ApprovalInput) (*dynamic.ApprovalRequest, error) {
			captured = in
			return created, nil
		},
	}

	env := unmarshalApprovalRequest(t, executeApprovalRequest(context.Background(), inv, []byte(`{
		"model": "PriceOverride",
		"record_id": "sku-1",
		"label": "Below floor price",
		"roles": ["sales_manager"],
		"reason_required": true,
		"expires_hours": 24,
		"payload": {"op": "wasm_callback", "sku": "sku-1", "price": 9.99},
		"snapshot": {"price": 12.5}
	}`)))

	if !env.Success {
		t.Fatalf("expected success, got %+v", env)
	}
	if env.Data.ID != created.ID.String() || env.Data.Status != dynamic.ApprovalStatusPending {
		t.Fatalf("unexpected data: %+v", env.Data)
	}
	if captured.AddonKey != "pricing" {
		t.Fatalf("addon key must come from the invocation, got %q", captured.AddonKey)
	}
	if captured.OrgID != orgID {
		t.Fatalf("orgID must come from the invocation, got %v", captured.OrgID)
	}
	if captured.Kind != dynamic.ApprovalKindExplicit {
		t.Fatalf("expected explicit kind, got %q", captured.Kind)
	}
	if len(captured.Roles) != 1 || captured.Roles[0] != "sales_manager" {
		t.Fatalf("unexpected roles: %v", captured.Roles)
	}
}
