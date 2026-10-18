package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

// approval_consume host import limits (docs/wasm-abi.md § 23).
const (
	approvalConsumeMaxReqBytes = 8 * 1024
	approvalConsumeDeadline    = 5 * time.Second
)

// ApprovalConsumeEnvelopeVersion is the wire-format version of the envelope
// returned by `metacore_host.approval_consume`.
const ApprovalConsumeEnvelopeVersion = 1

// approvalConsumePayload is the guest request. Org and the acting user are
// deliberately absent: both come from the invocation context.
type approvalConsumePayload struct {
	ApprovalID string `json:"approval_id"`
	Policy     string `json:"policy"`
	// ContextEq / ContextGte constrain the context the supervisor authorized
	// (see dynamic.ConsumeInput).
	ContextEq  map[string]string  `json:"context_eq"`
	ContextGte map[string]float64 `json:"context_gte"`
}

// executeApprovalConsume redeems one PIN grant. Failure codes: invalid_request,
// approvals_unavailable, approval_invalid (the grant is not redeemable; message
// carries the reason), db_error.
func executeApprovalConsume(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey := ""
	orgID := uuid.Nil
	if inv != nil {
		addonKey = inv.addonKey
		orgID = inv.orgID
	}
	fail := func(code, msg string) []byte {
		return approvalConsumeErr(addonKey, code, msg, orgID, start)
	}
	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if inv.approvalConsumer == nil {
		return fail("approvals_unavailable", "host has no approval consumer configured")
	}
	if len(reqJSON) == 0 || len(reqJSON) > approvalConsumeMaxReqBytes {
		return fail("invalid_request", fmt.Sprintf("request must be 1..%d bytes", approvalConsumeMaxReqBytes))
	}
	if orgID == uuid.Nil {
		return fail("invalid_request", "invocation has no bound orgID")
	}
	var req approvalConsumePayload
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	id, err := uuid.Parse(req.ApprovalID)
	if err != nil || req.Policy == "" {
		return fail("invalid_request", "approval_id (uuid) and policy are required")
	}
	actor, err := uuid.Parse(dynamic.ActorIDFromContext(ctx))
	if err != nil {
		return fail("invalid_request", "invocation has no acting user")
	}
	execCtx, cancel := context.WithTimeout(ctx, approvalConsumeDeadline)
	defer cancel()
	err = inv.approvalConsumer(execCtx, dynamic.ConsumeInput{
		OrgID: orgID, ApprovalID: id, Policy: req.Policy, RequestedBy: actor,
		ContextEq: req.ContextEq, ContextGte: req.ContextGte,
	})
	if err != nil {
		if errors.Is(err, dynamic.ErrApprovalConsume) || errors.Is(err, dynamic.ErrApprovalGrantRequired) {
			return fail("approval_invalid", err.Error())
		}
		return fail("db_error", err.Error())
	}
	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    map[string]any{"consumed": true},
		"meta":    approvalConsumeMeta(addonKey, orgID, start),
	})
	return env
}

func approvalConsumeMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": ApprovalConsumeEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

func approvalConsumeErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   hostErrorBody(code, message),
		"meta":    approvalConsumeMeta(addonKey, orgID, start),
	})
	return b
}
