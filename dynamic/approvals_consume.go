package dynamic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
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
	// AddonKey / ModelKey / RecordID anchor the redemption to the document the
	// grant was issued for: when the grant carries one of them it MUST match
	// (same rule as ConsumePINGrant's record anchor). A grant anchored to record
	// R cannot be spent on anything else.
	AddonKey string
	ModelKey string
	RecordID string
	// DB is the handle the redemption runs on. Pass the open transaction of the
	// write the grant pays for, so a rollback leaves the grant unspent. nil =
	// the service's own connection (the redemption commits on its own).
	DB *gorm.DB
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
	// A redemption with no context to check would let any grant of the policy
	// pay for anything: require at least one anchor on the authorized context.
	if len(in.ContextEq)+len(in.ContextGte) == 0 {
		return refuse("context required")
	}
	if err := s.ensureApprovalTable(); err != nil {
		return err
	}
	db := in.DB
	if db == nil {
		db = s.db
	}
	db = db.WithContext(ctx)
	var row ApprovalRequest
	if err := db.Where("id = ? AND organization_id = ?", in.ApprovalID, in.OrgID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return refuse("unknown grant")
		}
		// A failing database is an internal error, not a refusal.
		return err
	}
	// Ownership first: every refusal about a grant that is not the caller's
	// reads exactly like a grant that does not exist (no oracle on ids/state).
	if row.RequestedBy != in.RequestedBy {
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
	case row.AppliedEventID != "":
		return refuse("already used")
	case time.Since(*row.DecidedAt) > PINGrantMaxAge:
		return refuse("expired")
	case row.AddonKey != "" && row.AddonKey != in.AddonKey,
		row.ModelKey != "" && row.ModelKey != in.ModelKey,
		row.RecordID != "" && row.RecordID != in.RecordID:
		return refuse("grant anchored to another document")
	}
	// The recorded context is immutable once the grant is applied, so checking
	// it on the read row is sound.
	recorded, _ := row.PayloadMap()["context"].(map[string]any)
	for k, want := range in.ContextEq {
		if !contextEquals(recorded[k], want) {
			return refuse("context mismatch: " + k)
		}
	}
	for k, min := range in.ContextGte {
		if math.IsNaN(min) || math.IsInf(min, 0) {
			return refuse("invalid amount: " + k)
		}
		// An absent or non-numeric recorded value covers nothing (it is not 0).
		got, ok := numberOf(recorded[k])
		if !ok || got < min-0.01 {
			return refuse("grant does not cover: " + k)
		}
	}
	// Atomic single use. Org, kind, policy, status, owner, freshness and the unused
	// marker are all in the WHERE: only a row that STILL satisfies every condition
	// flips from '' to "consumed".
	res := db.Model(&ApprovalRequest{}).
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

// numberOf reads a JSON-decoded number (float64) or numeric string. ok=false
// for an absent, non-numeric or non-finite value.
func numberOf(v any) (float64, bool) {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case int:
		f = float64(t)
	case string:
		// ParseFloat accepts "Inf", "Infinity" and "NaN": a non-finite recorded
		// amount must never cover anything.
		p, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		f = p
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// contextEquals compares a recorded context value with its expected text. When
// both sides are finite numbers they compare as numbers, so a float64 recorded
// as 1e6 matches "1000000" (fmt.Sprint would print "1e+06").
func contextEquals(recorded any, want string) bool {
	if recorded == nil {
		return false
	}
	if wn, err := strconv.ParseFloat(strings.TrimSpace(want), 64); err == nil && !math.IsNaN(wn) && !math.IsInf(wn, 0) {
		if rn, ok := numberOf(recorded); ok {
			// Codes such as "007" are identifiers, not amounts: a string with a
			// leading zero only matches textually.
			if str, isStr := recorded.(string); !isStr || !hasLeadingZero(str) {
				return rn == wn
			}
		}
	}
	return fmt.Sprint(recorded) == want
}

func hasLeadingZero(s string) bool {
	s = strings.TrimLeft(strings.TrimSpace(s), "+-")
	return len(s) > 1 && s[0] == '0' && s[1] != '.'
}
