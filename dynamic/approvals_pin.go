package dynamic

// approvals_pin.go — supervisor PIN override for the approvals primitive.
//
// A cashier at the till cannot wait for an async inbox decision: the customer is
// standing there. Two synchronous paths let a supervisor authorize on the spot by
// typing a PIN, both resolved through the host-supplied ApprovalPINVerifier (the
// kernel never stores PINs — the host owns credentials):
//
//   - GrantPINApproval: an inline authorization for a policy the CLIENT already
//     enforces (POS "sell without stock", "discount above X%", "price below the
//     floor", "refund"). Nothing is parked or replayed; the kernel verifies the
//     PIN, persists an audit row (kind=pin, status=applied: who asked, who
//     authorized, why, context) and emits `approval.granted`. The client gets the
//     row id back to stamp on the document it then writes.
//   - ApproveRequestWithPIN: decide an already-PARKED request (the 422
//     approval_required flow) with a supervisor PIN instead of waiting for the
//     inbox. The PIN owner becomes the decider, so the normal role gate, reason
//     rule and replay apply unchanged.
//
// Brute force: failed PINs are throttled per (org, requester) in memory
// (pinMaxFailures within pinLockWindow), on top of whatever the verifier does.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// ApprovalKindPIN marks an audit row written by GrantPINApproval.
const ApprovalKindPIN = "pin"

// ApprovalPINOp is the payload `op` of a PIN grant row. It has no applier: the
// row is born terminal (applied), there is nothing to replay.
const ApprovalPINOp = "pin_grant"

const (
	pinMaxFailures = 5
	pinLockWindow  = 5 * time.Minute
)

var (
	// ErrApprovalPINUnavailable: the host wired no ApprovalPINVerifier. HTTP 501.
	ErrApprovalPINUnavailable = errors.New("supervisor PIN approval is not configured")
	// ErrApprovalPINInvalid: no eligible supervisor matches the PIN. HTTP 403.
	// Deliberately says nothing about WHY (unknown PIN vs. wrong role).
	ErrApprovalPINInvalid = fmt.Errorf("%w: invalid supervisor PIN", ErrForbidden)
	// ErrApprovalPINLocked: too many failed attempts. HTTP 429.
	ErrApprovalPINLocked = errors.New("too many failed PIN attempts, try again later")
	// ErrApprovalPINReasonRequired: a PIN grant always carries a reason (audit).
	ErrApprovalPINReasonRequired = ErrApprovalReasonRequired
)

// ApprovalPINVerifier resolves a supervisor PIN to the approver principal for a
// policy. policyKey is the client-declared policy of a GrantPINApproval
// ("pos.oversell", "pos.discount"…) or "" when deciding a parked request (the
// kernel then matches the approver's roles against the request's roles). It
// returns ErrApprovalPINInvalid — or any error — when nobody eligible matches;
// the approver MUST belong to orgID. See Config.ApprovalPINVerifier.
type ApprovalPINVerifier func(ctx context.Context, orgID uuid.UUID, policyKey, pin string) (modelbase.AuthUser, error)

// PINGrantInput is the body of GrantPINApproval.
type PINGrantInput struct {
	// PolicyKey names the sensitive action being authorized. Required.
	PolicyKey string
	// Label is the human title shown in the audit trail (defaults to PolicyKey).
	Label string
	// Reason is mandatory: why the supervisor lets it through.
	Reason string
	PIN    string
	// AddonKey / ModelKey / RecordID optionally anchor the audit row to a document.
	AddonKey string
	ModelKey string
	RecordID string
	// Context is free-form audit data (product, quantities, percentages, before →
	// after). It is stored verbatim in the payload.
	Context map[string]any
}

type pinThrottle struct {
	failures int
	first    time.Time
}

func pinThrottleKey(org, requester uuid.UUID) string { return org.String() + "/" + requester.String() }

func (s *Service) pinLocked(key string) bool {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	t := s.pinThrottles[key]
	if t == nil {
		return false
	}
	if time.Since(t.first) > pinLockWindow {
		delete(s.pinThrottles, key)
		return false
	}
	return t.failures >= pinMaxFailures
}

func (s *Service) pinFailed(key string) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	if s.pinThrottles == nil {
		s.pinThrottles = map[string]*pinThrottle{}
	}
	t := s.pinThrottles[key]
	if t == nil || time.Since(t.first) > pinLockWindow {
		t = &pinThrottle{first: time.Now()}
		s.pinThrottles[key] = t
	}
	t.failures++
}

func (s *Service) pinOK(key string) {
	s.approvalMu.Lock()
	delete(s.pinThrottles, key)
	s.approvalMu.Unlock()
}

// verifyPIN runs the throttle + the host verifier and enforces that the
// approver belongs to the requester's org.
func (s *Service) verifyPIN(ctx context.Context, requester modelbase.AuthUser, policyKey, pin string) (modelbase.AuthUser, error) {
	if s.approvalPINVerifier == nil {
		return nil, ErrApprovalPINUnavailable
	}
	if requester == nil {
		return nil, ErrForbidden
	}
	orgID := orgIDFromUser(requester)
	if orgID == uuid.Nil {
		return nil, ErrTenantScopeUnavailable
	}
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return nil, ErrApprovalPINInvalid
	}
	key := pinThrottleKey(orgID, requester.GetID())
	if s.pinLocked(key) {
		return nil, ErrApprovalPINLocked
	}
	approver, err := s.approvalPINVerifier(ctx, orgID, policyKey, pin)
	if err != nil || approver == nil || orgIDFromUser(approver) != orgID {
		s.pinFailed(key)
		return nil, ErrApprovalPINInvalid
	}
	s.pinOK(key)
	return approver, nil
}

// GrantPINApproval verifies a supervisor PIN for an inline authorization and
// records the audit row. See the file header.
func (s *Service) GrantPINApproval(ctx context.Context, requester modelbase.AuthUser, in PINGrantInput) (*ApprovalRequest, error) {
	policy := strings.TrimSpace(in.PolicyKey)
	if policy == "" {
		return nil, fmt.Errorf("%w: policy is required", ErrInvalidInput)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, ErrApprovalPINReasonRequired
	}
	approver, err := s.verifyPIN(ctx, requester, policy, in.PIN)
	if err != nil {
		return nil, err
	}
	if err := s.ensureApprovalTable(); err != nil {
		return nil, err
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		label = policy
	}
	now := time.Now().UTC()
	approverID := approver.GetID()
	roles := s.actorRoles(ctx, approver)
	req := &ApprovalRequest{
		ID:              uuid.New(),
		OrganizationID:  orgIDFromUser(requester),
		AddonKey:        in.AddonKey,
		ModelKey:        in.ModelKey,
		RecordID:        in.RecordID,
		ConstraintKey:   policy,
		Kind:            ApprovalKindPIN,
		Label:           label,
		Status:          ApprovalStatusApplied,
		RequestedBy:     requester.GetID(),
		RequestedByRole: requester.GetRole(),
		RequestedAt:     now,
		Roles:           mustJSON(roles),
		ReasonRequired:  true,
		Payload:         mustJSON(map[string]any{"op": ApprovalPINOp, "policy": policy, "context": in.Context}),
		Snapshot:        "null",
		Violation:       "null",
		DecidedBy:       &approverID,
		DecidedAt:       &now,
		Reason:          reason,
		Result:          mustJSON(map[string]any{"granted": true}),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.db.WithContext(ctx).Create(req).Error; err != nil {
		return nil, fmt.Errorf("dynamic: pin approval: %w", err)
	}
	s.publishApprovalEvent(ctx, "granted", req, approver)
	return req, nil
}

// ApproveRequestWithPIN decides a parked request as the owner of a supervisor
// PIN and replays the mutation (ApproveRequest semantics: role gate, reason,
// replay on behalf of the original requester).
func (s *Service) ApproveRequestWithPIN(ctx context.Context, requester modelbase.AuthUser, id uuid.UUID, pin, reason string) (*ApprovalRequest, error) {
	if _, err := s.loadApproval(ctx, requester, id); err != nil {
		return nil, err
	}
	approver, err := s.verifyPIN(ctx, requester, "", pin)
	if err != nil {
		return nil, err
	}
	return s.ApproveRequest(ctx, approver, id, reason)
}
