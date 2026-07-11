package dynamic

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ActionIdempotencyRecord is the kernel-owned table backing declarative action
// idempotency (manifest v3 Action.idempotency). One row per
// (org, model, action, key); the serialized ActionResult is replayed verbatim
// on any repeat of that tuple, so a payment capture or a fiscal stamp survives a
// client retry without re-dispatching the handler.
//
// It follows the same "kernel-owned gorm model, lazily auto-migrated" pattern as
// dynamic.Migration (metacore_addon_migrations): no bundle migration, the table
// is ensured on first use.
type ActionIdempotencyRecord struct {
	ID        uint64    `gorm:"primaryKey"`
	OrgID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_metacore_action_idem"`
	Model     string    `gorm:"size:200;not null;uniqueIndex:idx_metacore_action_idem"`
	Action    string    `gorm:"size:200;not null;uniqueIndex:idx_metacore_action_idem"`
	Key       string    `gorm:"column:idem_key;size:255;not null;uniqueIndex:idx_metacore_action_idem"`
	Response  []byte    `gorm:"type:jsonb"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

// TableName pins the physical table name (public schema).
func (ActionIdempotencyRecord) TableName() string { return "metacore_action_idempotency" }

// storedActionResult is the serialized shape of an ActionResult persisted for
// replay. HTTPStatus rides along so the replayed response is byte-identical to
// the original, including the suggested status code.
type storedActionResult struct {
	Success    bool           `json:"success"`
	Data       any            `json:"data,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Error      *ActionError   `json:"error,omitempty"`
	HTTPStatus int            `json:"httpStatus"`
}

// idempotencyMigrated tracks which *gorm.DB handles have had the idempotency
// table ensured, so AutoMigrate runs at most once per process per DB.
var (
	idempotencyMu       sync.Mutex
	idempotencyMigrated = map[*gorm.DB]bool{}
)

func ensureIdempotencyTable(db *gorm.DB) error {
	idempotencyMu.Lock()
	defer idempotencyMu.Unlock()
	if idempotencyMigrated[db] {
		return nil
	}
	if err := db.AutoMigrate(&ActionIdempotencyRecord{}); err != nil {
		return fmt.Errorf("migrate metacore_action_idempotency: %w", err)
	}
	idempotencyMigrated[db] = true
	return nil
}

// idemReservation is the outcome of reserveIdempotent: exactly one of the
// three states holds.
type idemReservation int

const (
	// idemReserved — this caller won the slot and MUST dispatch, then either
	// completeIdempotent (success) or releaseIdempotent (failure).
	idemReserved idemReservation = iota
	// idemReplay — a finished response exists; replay it, do not dispatch.
	idemReplay
	// idemInFlight — another invocation holds the slot and has not finished;
	// the caller must NOT dispatch (409, retryable).
	idemInFlight
)

// reserveIdempotent claims the (org, model, action, key) slot BEFORE the
// dispatch, closing the classic check-then-act race: the INSERT with an empty
// Response is the reservation, and the unique index arbitrates concurrent
// claimants — exactly one wins, every simultaneous loser sees the pending row
// and backs off with idemInFlight instead of double-dispatching a payment
// capture. It ensures the backing table on first use.
func (s *Service) reserveIdempotent(ctx context.Context, orgID uuid.UUID, model, action, key string) (idemReservation, ActionResult, error) {
	if err := ensureIdempotencyTable(s.db); err != nil {
		return idemInFlight, ActionResult{}, err
	}
	rec := ActionIdempotencyRecord{OrgID: orgID, Model: model, Action: action, Key: key}
	res := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&rec)
	if res.Error != nil {
		return idemInFlight, ActionResult{}, res.Error
	}
	if res.RowsAffected == 1 {
		return idemReserved, ActionResult{}, nil
	}

	// Lost the insert — someone holds (or held) the slot. Load it.
	var existing ActionIdempotencyRecord
	err := s.db.WithContext(ctx).
		Where("org_id = ? AND model = ? AND action = ? AND idem_key = ?", orgID, model, action, key).
		First(&existing).Error
	if err != nil {
		if isNotFound(err) {
			// The holder released between our insert and this read (its dispatch
			// failed). Retryable — surface as in-flight so the client re-sends.
			return idemInFlight, ActionResult{}, nil
		}
		return idemInFlight, ActionResult{}, err
	}
	if len(existing.Response) == 0 {
		// Reservation without a response: the first invocation is still running.
		return idemInFlight, ActionResult{}, nil
	}
	var st storedActionResult
	if uerr := json.Unmarshal(existing.Response, &st); uerr != nil {
		// A corrupt row must not wedge the action forever — drop it and report
		// in-flight so the NEXT retry re-reserves cleanly.
		_ = s.releaseIdempotent(ctx, orgID, model, action, key)
		return idemInFlight, ActionResult{}, nil
	}
	replay := ActionResult{
		Success:    st.Success,
		Data:       st.Data,
		Meta:       st.Meta,
		Error:      st.Error,
		HTTPStatus: st.HTTPStatus,
	}
	if replay.Meta == nil {
		replay.Meta = map[string]any{}
	}
	replay.Meta["idempotent_replay"] = true
	return idemReplay, replay, nil
}

// completeIdempotent finishes a reservation by attaching the response that
// every later retry will replay.
func (s *Service) completeIdempotent(ctx context.Context, orgID uuid.UUID, model, action, key string, res ActionResult) error {
	body, err := json.Marshal(storedActionResult{
		Success:    res.Success,
		Data:       res.Data,
		Meta:       res.Meta,
		Error:      res.Error,
		HTTPStatus: res.HTTPStatus,
	})
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).
		Model(&ActionIdempotencyRecord{}).
		Where("org_id = ? AND model = ? AND action = ? AND idem_key = ?", orgID, model, action, key).
		Update("response", body).Error
}

// releaseIdempotent frees a reservation whose dispatch failed or was declined,
// so a later retry gets to run the handler again (only SUCCESSFUL results are
// cached — a declined payment stays retryable).
func (s *Service) releaseIdempotent(ctx context.Context, orgID uuid.UUID, model, action, key string) error {
	return s.db.WithContext(ctx).
		Where("org_id = ? AND model = ? AND action = ? AND idem_key = ?", orgID, model, action, key).
		Delete(&ActionIdempotencyRecord{}).Error
}

// idempotencyKeyFromPayload extracts and stringifies the idempotency key from
// the action payload. An absent / empty / non-scalar value yields "" — the
// caller then dispatches with no replay guarantee (documented behaviour).
func idempotencyKeyFromPayload(payload map[string]any, keyField string) string {
	if payload == nil || keyField == "" {
		return ""
	}
	v, ok := payload[keyField]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers arrive as float64. Render in plain decimal notation
		// (never scientific) with the minimal digits that round-trip, so the
		// same payload number always yields the same key string.
		return strconv.FormatFloat(t, 'f', -1, 64)
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}
