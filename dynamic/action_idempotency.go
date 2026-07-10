package dynamic

import (
	"context"
	"encoding/json"
	"fmt"
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

// lookupIdempotentResult returns the stored ActionResult for the tuple, or
// ok=false when none exists yet. It ensures the backing table on first use.
func (s *Service) lookupIdempotentResult(ctx context.Context, orgID uuid.UUID, model, action, key string) (ActionResult, bool, error) {
	if err := ensureIdempotencyTable(s.db); err != nil {
		return ActionResult{}, false, err
	}
	var rec ActionIdempotencyRecord
	err := s.db.WithContext(ctx).
		Where("org_id = ? AND model = ? AND action = ? AND idem_key = ?", orgID, model, action, key).
		First(&rec).Error
	if err != nil {
		if isNotFound(err) {
			return ActionResult{}, false, nil
		}
		return ActionResult{}, false, err
	}
	var st storedActionResult
	if uerr := json.Unmarshal(rec.Response, &st); uerr != nil {
		// A corrupt row must not wedge the action forever — treat it as a miss
		// so the handler re-dispatches and the row is overwritten below.
		return ActionResult{}, false, nil
	}
	res := ActionResult{
		Success:    st.Success,
		Data:       st.Data,
		Meta:       st.Meta,
		Error:      st.Error,
		HTTPStatus: st.HTTPStatus,
	}
	if res.Meta == nil {
		res.Meta = map[string]any{}
	}
	res.Meta["idempotent_replay"] = true
	return res, true, nil
}

// storeIdempotentResult persists the result for later replay. A concurrent
// duplicate (unique-index conflict) is ignored: the first committed response
// wins and this call becomes a no-op, which is the correct at-least-once
// semantics for a retry guard. NOTE: this is single-transaction dedup, not a
// distributed lock — two simultaneous in-flight invocations with the same key
// may both dispatch before either stores; the persisted replay only guarantees
// that every LATER retry returns the first stored response.
func (s *Service) storeIdempotentResult(ctx context.Context, orgID uuid.UUID, model, action, key string, res ActionResult) error {
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
	rec := ActionIdempotencyRecord{OrgID: orgID, Model: model, Action: action, Key: key, Response: body}
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&rec).Error
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
		// JSON numbers arrive as float64; render without trailing zeros.
		return fmt.Sprintf("%v", t)
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}
