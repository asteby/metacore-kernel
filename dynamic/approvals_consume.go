package dynamic

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrApprovalConsume is the base of every refusal ConsumeApproval returns. The
// wrapped message names the first condition that failed, so a caller can show
// WHY a grant was refused without distinguishing "not found" from "not yours"
// (those two stay one message on purpose: no oracle for guessing ids).
var ErrApprovalConsume = fmt.Errorf("%w: approval grant not redeemable", ErrApprovalGrantRequired)

// ConsumeInput describes one redemption of a PIN grant by a document writer
// that is not a supervised Action (e.g. a wasm handler writing sale lines).
// OrgID and RequestedBy always come from the host, never from the guest.
type ConsumeInput struct {
	OrgID       uuid.UUID
	ApprovalID  uuid.UUID
	Policy      string
	RequestedBy uuid.UUID
	// ContextEq: every key must equal the grant's recorded context value
	// (compared as text). Anchors the grant to a product / document.
	ContextEq map[string]string
	// ContextGte: every key's recorded numeric context value must be >= the
	// given amount (minus a one-cent tolerance). Proves the grant covers the
	// shortfall / discount being redeemed.
	ContextGte map[string]float64
}

// ConsumeApproval redeems a kind=pin grant for ONE use, atomically. It is the
// generic sibling of ConsumePINGrant for writers that know more about the grant
// than "policy + record": it additionally checks ContextEq / ContextGte against
// the context the supervisor authorized. The single UPDATE flips applied_event_id
// from empty to "consumed"; a second call, or two racing calls, lose.
func (s *Service) ConsumeApproval(ctx context.Context, in ConsumeInput) error {
	refuse := func(why string) error { return fmt.Errorf("%w: %s", ErrApprovalConsume, why) }
	if in.OrgID == uuid.Nil {
		return ErrTenantScopeUnavailable
	}
	if in.ApprovalID == uuid.Nil || strings.TrimSpace(in.Policy) == "" || in.RequestedBy == uuid.Nil {
		return refuse("incomplete request")
	}
	if err := s.ensureApprovalTable(); err != nil {
		return err
	}
	var row ApprovalRequest
	if err := s.db.WithContext(ctx).Where("id = ? AND organization_id = ?", in.ApprovalID, in.OrgID).First(&row).Error; err != nil {
		return refuse("unknown grant")
	}
	switch {
	case row.Kind != ApprovalKindPIN || row.ConstraintKey != in.Policy:
		return refuse("wrong policy")
	case row.Status != ApprovalStatusApplied || row.DecidedAt == nil:
		return refuse("not authorized")
	case row.RequestedBy != in.RequestedBy:
		return refuse("requested by someone else")
	case row.AppliedEventID != "":
		return refuse("already used")
	case time.Since(*row.DecidedAt) > PINGrantMaxAge:
		return refuse("expired")
	}
	recorded, _ := row.PayloadMap()["context"].(map[string]any)
	for k, want := range in.ContextEq {
		if got := fmt.Sprint(recorded[k]); recorded[k] == nil || got != want {
			return refuse("context mismatch: " + k)
		}
	}
	for k, min := range in.ContextGte {
		if numberOf(recorded[k]) < min-0.01 {
			return refuse("grant does not cover: " + k)
		}
	}
	res := s.db.WithContext(ctx).Model(&ApprovalRequest{}).
		Where("id = ? AND organization_id = ? AND (applied_event_id = '' OR applied_event_id IS NULL)", in.ApprovalID, in.OrgID).
		Update("applied_event_id", "consumed")
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return refuse("already used")
	}
	return nil
}

// numberOf reads a JSON-decoded number (float64) or numeric string; 0 otherwise.
func numberOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return 0
}
