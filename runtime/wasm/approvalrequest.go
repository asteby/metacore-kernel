package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

// approval_request host import limits (docs/wasm-abi.md § 19). Mirrors the
// other write-path imports: a small request (the payload is the mutation the
// guest wants replayed on approval, not the mutation itself) and a small
// response (just the parked request's id + status).
const (
	approvalRequestMaxReqBytes  = 32 * 1024
	approvalRequestMaxRespBytes = 32 * 1024
	approvalRequestDeadline     = 5 * time.Second
)

// ApprovalRequestEnvelopeVersion is the wire-format version of the JSON
// envelope returned by `metacore_host.approval_request`.
const ApprovalRequestEnvelopeVersion = 1

// approvalRequestPayload is the guest-supplied JSON request. `organization_id`
// and `addon_key` are deliberately absent — tenant scope and the requesting
// addon ALWAYS come from the invocation context, never from the guest (same
// rule as data_mutate / event_emit).
type approvalRequestPayload struct {
	Model          string         `json:"model"`
	RecordID       string         `json:"record_id"`
	Label          string         `json:"label"`
	Roles          []string       `json:"roles"`
	ReasonRequired bool           `json:"reason_required"`
	ExpiresHours   int            `json:"expires_hours"`
	Payload        map[string]any `json:"payload"`
	Snapshot       map[string]any `json:"snapshot"`
	Violation      map[string]any `json:"violation"`
}

// executeApprovalRequest is the inner pure-Go path the
// `metacore_host.approval_request` import calls into. It parks a pending
// dynamic.ApprovalRequest of kind `explicit` — a wasm handler that decided,
// on its own logic (not a declarative column guard), that a mutation it is
// about to make needs a human approver before it runs. The handler supplies
// its own replay `payload` (an `op` the embedder's ApprovalApplier registry
// knows how to re-execute, e.g. "wasm_callback") — this import never performs
// a mutation itself, it only records the intent to. All failures surface
// inside the JSON envelope; the function never returns an error (wire shape:
// `{success, data, meta}` — docs/wasm-abi.md § 14 / § 19).
func executeApprovalRequest(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey := ""
	orgID := uuid.Nil
	if inv != nil {
		addonKey = inv.addonKey
		orgID = inv.orgID
	}
	fail := func(code, msg string) []byte {
		return approvalRequestErr(addonKey, code, msg, orgID, start)
	}

	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if inv.approvalRequester == nil {
		return fail("approvals_unavailable", "host has no approval requester configured")
	}
	if len(reqJSON) == 0 {
		return fail("invalid_request", "empty request")
	}
	if len(reqJSON) > approvalRequestMaxReqBytes {
		return fail("invalid_request",
			fmt.Sprintf("request exceeds %d byte cap", approvalRequestMaxReqBytes))
	}
	if orgID == uuid.Nil {
		return fail("invalid_request", "invocation has no bound orgID")
	}

	var req approvalRequestPayload
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	if len(req.Roles) == 0 {
		return fail("invalid_request", "roles must include at least one approver role")
	}
	op, _ := req.Payload["op"].(string)
	if op == "" {
		return fail("invalid_request", "payload.op is required")
	}

	execCtx, cancel := context.WithTimeout(ctx, approvalRequestDeadline)
	defer cancel()

	actorID := uuid.Nil
	if id, err := uuid.Parse(dynamic.ActorIDFromContext(ctx)); err == nil {
		actorID = id
	}

	in := dynamic.ApprovalInput{
		Kind:           dynamic.ApprovalKindExplicit,
		AddonKey:       addonKey,
		ModelKey:       req.Model,
		Model:          req.Model,
		RecordID:       req.RecordID,
		Label:          req.Label,
		OrgID:          orgID,
		RequestedBy:    actorID,
		Roles:          req.Roles,
		ReasonRequired: req.ReasonRequired,
		ExpiresHours:   req.ExpiresHours,
		Payload:        req.Payload,
		Snapshot:       req.Snapshot,
		Violation:      req.Violation,
	}
	created, err := inv.approvalRequester(execCtx, in)
	if err != nil {
		return fail("db_error", err.Error())
	}

	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data": map[string]any{
			"id":     created.ID.String(),
			"status": created.Status,
		},
		"meta": approvalRequestMeta(addonKey, orgID, start),
	})
	if len(env) > approvalRequestMaxRespBytes {
		return fail("db_error", "response exceeds size cap")
	}
	return env
}

func approvalRequestMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": ApprovalRequestEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

// approvalRequestErr builds the failure envelope per docs/wasm-abi.md § 19.5.
// `code` is one of: invalid_request | approvals_unavailable | db_error.
func approvalRequestErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": message},
		"meta":    approvalRequestMeta(addonKey, orgID, start),
	})
	return b
}
