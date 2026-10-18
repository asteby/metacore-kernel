package dynamic

import (
	"context"
	"fmt"
	"math"
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
	// Early, readable refusals on the row we read. They are advisory: the
	// UPDATE below re-checks every one of them atomically, so a row that changes
	// between this read and the redemption (status, owner, age, usage) still loses.
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
	// The recorded context is immutable once the grant is applied, so checking
	// it on the read row is sound.
	recorded, _ := row.PayloadMap()["context"].(map[string]any)
	for k, want := range in.ContextEq {
		if got := fmt.Sprint(recorded[k]); recorded[k] == nil || got != want {
			return refuse("context mismatch: " + k)
		}
	}
	for k, min := range in.ContextGte {
		if math.IsNaN(min) || math.IsInf(min, 0) {
			return refuse("invalid amount: " + k)
		}
		if numberOf(recorded[k]) < min-0.01 {
			return refuse("grant does not cover: " + k)
		}
	}
	// Atomic single use. Org, kind, policy, status, owner, freshness and the unused
	// marker are all in the WHERE: only a row that STILL satisfies every condition
	// flips from '' to "consumed".
	res := s.db.WithContext(ctx).Model(&ApprovalRequest{}).
		Where("id = ? AND organization_id = ? AND kind = ? AND constraint_key = ? AND status = ? AND requested_by = ?",
			in.ApprovalID, in.OrgID, ApprovalKindPIN, in.Policy, ApprovalStatusApplied, in.RequestedBy).
		Where("decided_at IS NOT NULL AND decided_at > ?", time.Now().Add(-PINGrantMaxAge)).
		Where("(applied_event_id = '' OR applied_event_id IS NULL)").
		Update("applied_event_id", "consumed")
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return refuse("no longer redeemable")
	}
	return nil
}

// numberOf reads a JSON-decoded number (float64) or numeric string; 0 otherwise.
func numberOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0
		}
		return t
	case int:
		return float64(t)
	case string:
		// ParseFloat accepts "Inf", "Infinity" and "NaN": a non-finite recorded
		// amount must never cover anything.
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	}
	return 0
}
