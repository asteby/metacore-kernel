package inbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Receipt statuses.
const (
	StatusProcessing = "processing"
	StatusApplied    = "applied"
	StatusFailed     = "failed"
	StatusRejected   = "rejected" // schema/key invalid; never reserved
)

// Receipt is one row of the reception log. It never holds the payload: only
// its hash, the contract, the outcome and (opt-in) the event key. Personal
// data in the event key is stored hashed unless the contract opts in.
type Receipt struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	OrgID     uuid.UUID `gorm:"type:uuid;not null;index:idx_inbound_org_created,priority:1" json:"organization_id"`
	Contract  string    `gorm:"size:128;not null;index" json:"contract"`
	Version   int       `json:"version"`
	Status    string    `gorm:"size:16;not null;index" json:"status"`
	KeyHash   string    `gorm:"size:64" json:"key_hash,omitempty"`
	EventKey  string    `gorm:"size:256" json:"event_key,omitempty"`
	BodyHash  string    `gorm:"size:64" json:"body_hash"`
	RecordID  string    `gorm:"size:64" json:"record_id,omitempty"`
	ErrorCode string    `gorm:"size:64" json:"error_code,omitempty"`
	Attempts  int       `json:"attempts"`
	// Shadow marks a dry-run receipt (contract with dry_run, or the panel test): it
	// holds what the event WOULD have done in Progress and wrote nothing. Its
	// claim lives in its own key space (shadowKeyHash), so it never blocks, and is
	// never mistaken for, the real application of the same event.
	Shadow bool `gorm:"not null;default:false" json:"shadow,omitempty"`
	// Progress is the per-action state of a chain (ids and codes only, never
	// the body); a retry resumes after the last finished action.
	Progress []ActionStep `gorm:"serializer:json;type:text" json:"actions,omitempty"`
	// Claim is org|contract|keyhash while the event key is reserved; NULL for
	// rejected rows. The unique index makes the reservation atomic.
	Claim     *string   `gorm:"size:256;uniqueIndex" json:"-"`
	CreatedAt time.Time `gorm:"index:idx_inbound_org_created,priority:2" json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is the log table.
func (Receipt) TableName() string { return "metacore_inbound_receipts" }

// ReceiptFilter narrows List. Org is mandatory (isolation).
type ReceiptFilter struct {
	Contract string
	Status   string
	// EventKey is the clear key; it is hashed to look it up.
	EventKey string
	// Shadow narrows to dry-run receipts (true) or real ones (false); nil = both.
	Shadow *bool
	Limit  int
}

// ReceiptStore is the reception log and idempotency ledger.
type ReceiptStore interface {
	// Begin reserves the event. claimed=true means the caller owns the
	// processing. Otherwise existing describes the earlier receipt.
	// A failed or stale (processing past lease) receipt is re-claimed with
	// Attempts+1.
	Begin(ctx context.Context, r Receipt, lease time.Duration) (cur Receipt, claimed bool, err error)
	// Finish records the outcome of a claimed receipt.
	Finish(ctx context.Context, id uuid.UUID, status, recordID, errCode string) error
	// SaveProgress persists the per-action state of a claimed receipt, fenced
	// by attempt (WHERE id AND attempts AND status=processing), and
	// renews its lease.
	SaveProgress(ctx context.Context, id uuid.UUID, attempt int, steps []ActionStep) (owned bool, err error)
	// Record stores a receipt that is not reserved (rejected events).
	Record(ctx context.Context, r Receipt) error
	// List returns the org's receipts, newest first.
	List(ctx context.Context, org uuid.UUID, f ReceiptFilter) ([]Receipt, error)
}

// DefaultMaxRejected is how many rejected rows are kept per organization.
const DefaultMaxRejected = 500

// ErrEventKeyNeedsContract: the key hash is scoped by contract, so searching
// by event key requires the contract (same rule in every store).
var ErrEventKeyNeedsContract = errors.New("inbound: EventKey filter requires Contract")

// AttemptFinisher is an optional ReceiptStore extension. FinishAttempt
// finishes the receipt only if attempt is still the owner (fencing) and
// reports whether it did. Both built-in stores implement it and the handler
// prefers it over Finish.
type AttemptFinisher interface {
	FinishAttempt(ctx context.Context, id uuid.UUID, attempt int, status, recordID, errCode string) (bool, error)
}

// HashKey is the stable hash used for event keys (scoped by org and contract).
func HashKey(org uuid.UUID, contract, key string) string {
	sum := sha256.Sum256([]byte(org.String() + "|" + contract + "|" + key))
	return hex.EncodeToString(sum[:])
}

// shadowKeyHash is the key hash of a dry-run event: another key space than
// HashKey, so the shadow reservation never collides with the real one.
func shadowKeyHash(org uuid.UUID, contract, key string) string {
	return HashKey(org, contract+"|shadow", key)
}

// keyHashes are the hashes an EventKey filter matches, per the Shadow filter.
func keyHashes(org uuid.UUID, contract, key string, shadow *bool) []string {
	switch {
	case shadow == nil:
		return []string{HashKey(org, contract, key), shadowKeyHash(org, contract, key)}
	case *shadow:
		return []string{shadowKeyHash(org, contract, key)}
	}
	return []string{HashKey(org, contract, key)}
}

func claimOf(r Receipt) string { return r.OrgID.String() + "|" + r.Contract + "|" + r.KeyHash }

// ---------------------------------------------------------------- memory ---

// MemoryReceiptStore is a process-local store for tests and single-replica
// hosts that do not need durability.
type MemoryReceiptStore struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]*Receipt
	claim map[string]uuid.UUID
	Now   func() time.Time
	// MaxRejected caps retained rejected rows per organization (default 500).
	MaxRejected int
}

// NewMemoryReceiptStore builds an empty store.
func NewMemoryReceiptStore() *MemoryReceiptStore {
	return &MemoryReceiptStore{rows: map[uuid.UUID]*Receipt{}, claim: map[string]uuid.UUID{}}
}

func (m *MemoryReceiptStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *MemoryReceiptStore) Begin(_ context.Context, r Receipt, lease time.Duration) (Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	c := claimOf(r)
	if id, ok := m.claim[c]; ok {
		cur := m.rows[id]
		stale := cur.Status == StatusProcessing && now.Sub(cur.UpdatedAt) > lease
		if (cur.Status == StatusFailed || stale) && resumeBlocked(*cur, r.BodyHash) {
			return *cur, false, nil
		}
		if cur.Status == StatusFailed || stale {
			cur.Status, cur.Attempts, cur.UpdatedAt, cur.ErrorCode = StatusProcessing, cur.Attempts+1, now, ""
			if !hasDone(cur.Progress) {
				cur.BodyHash = r.BodyHash
			}
			return *cur, true, nil
		}
		return *cur, false, nil
	}
	r.Status, r.Attempts, r.CreatedAt, r.UpdatedAt = StatusProcessing, 1, now, now
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	cc := c
	r.Claim = &cc
	m.rows[r.ID] = &r
	m.claim[c] = r.ID
	return r, true, nil
}

func (m *MemoryReceiptStore) Finish(_ context.Context, id uuid.UUID, status, recordID, errCode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return errors.New("receipt not found")
	}
	r.Status, r.RecordID, r.ErrorCode, r.UpdatedAt = status, recordID, errCode, m.now()
	return nil
}

func (m *MemoryReceiptStore) Record(_ context.Context, r Receipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.Claim, r.CreatedAt, r.UpdatedAt, r.Attempts = nil, now, now, 1
	m.rows[r.ID] = &r
	if r.Status == StatusRejected {
		m.pruneRejected(r.OrgID)
	}
	return nil
}

func (m *MemoryReceiptStore) pruneRejected(org uuid.UUID) {
	max := m.MaxRejected
	if max <= 0 {
		max = DefaultMaxRejected
	}
	var rows []*Receipt
	for _, x := range m.rows {
		if x.OrgID == org && x.Status == StatusRejected {
			rows = append(rows, x)
		}
	}
	if len(rows) <= max {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	for _, x := range rows[:len(rows)-max] {
		delete(m.rows, x.ID)
	}
}

// FinishAttempt is Finish fenced by the owning attempt number.
func (m *MemoryReceiptStore) FinishAttempt(_ context.Context, id uuid.UUID, attempt int, status, recordID, errCode string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.Attempts != attempt || r.Status != StatusProcessing {
		return false, nil
	}
	r.Status, r.RecordID, r.ErrorCode, r.UpdatedAt = status, recordID, errCode, m.now()
	return true, nil
}

func (m *MemoryReceiptStore) List(_ context.Context, org uuid.UUID, f ReceiptFilter) ([]Receipt, error) {
	if f.EventKey != "" && f.Contract == "" {
		return nil, ErrEventKeyNeedsContract
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Receipt
	for _, r := range m.rows {
		if r.OrgID != org || (f.Contract != "" && r.Contract != f.Contract) || (f.Status != "" && r.Status != f.Status) || (f.Shadow != nil && r.Shadow != *f.Shadow) {
			continue
		}
		if f.EventKey != "" && !slices.Contains(keyHashes(org, r.Contract, f.EventKey, f.Shadow), r.KeyHash) {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return limitReceipts(out, f.Limit), nil
}

func limitReceipts(rs []Receipt, n int) []Receipt {
	if n <= 0 || n > 500 {
		n = 100
	}
	if len(rs) > n {
		return rs[:n]
	}
	return rs
}

// ------------------------------------------------------------------ gorm ---

// GormReceiptStore persists receipts in metacore_inbound_receipts (Postgres;
// SQLite in tests). The unique claim index makes the reservation atomic across
// replicas.
type GormReceiptStore struct {
	db *gorm.DB
	// MaxRejected caps retained rejected rows per organization (default 500).
	MaxRejected int
}

// NewGormReceiptStore auto-migrates the table and returns the store.
func NewGormReceiptStore(db *gorm.DB) (*GormReceiptStore, error) {
	if err := db.AutoMigrate(&Receipt{}); err != nil {
		return nil, err
	}
	return &GormReceiptStore{db: db}, nil
}

func (g *GormReceiptStore) Begin(ctx context.Context, r Receipt, lease time.Duration) (Receipt, bool, error) {
	now := time.Now()
	r.ID, r.Status, r.Attempts, r.CreatedAt, r.UpdatedAt = uuid.New(), StatusProcessing, 1, now, now
	c := claimOf(r)
	r.Claim = &c
	res := g.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&r)
	if res.Error != nil {
		return Receipt{}, false, res.Error
	}
	if res.RowsAffected == 1 {
		return r, true, nil
	}
	var cur Receipt
	if err := g.db.WithContext(ctx).Where("claim = ?", c).First(&cur).Error; err != nil {
		return Receipt{}, false, err
	}
	stale := cur.Status == StatusProcessing && now.Sub(cur.UpdatedAt) > lease
	if cur.Status != StatusFailed && !stale {
		return cur, false, nil
	}
	if resumeBlocked(cur, r.BodyHash) {
		return cur, false, nil
	}
	// Re-claim atomically: only one racer matches the old (status, updated_at).
	up := g.db.WithContext(ctx).Model(&Receipt{}).
		Where("id = ? AND status = ? AND updated_at = ?", cur.ID, cur.Status, cur.UpdatedAt).
		Updates(map[string]any{"status": StatusProcessing, "attempts": cur.Attempts + 1,
			"updated_at": now, "error_code": "", "body_hash": r.BodyHash})
	if up.Error != nil {
		return Receipt{}, false, up.Error
	}
	if up.RowsAffected == 0 {
		return cur, false, nil
	}
	cur.Status, cur.Attempts, cur.UpdatedAt, cur.ErrorCode, cur.BodyHash = StatusProcessing, cur.Attempts+1, now, "", r.BodyHash
	return cur, true, nil
}

func (g *GormReceiptStore) Finish(ctx context.Context, id uuid.UUID, status, recordID, errCode string) error {
	return g.db.WithContext(ctx).Model(&Receipt{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "record_id": recordID, "error_code": errCode, "updated_at": time.Now()}).Error
}

func (g *GormReceiptStore) Record(ctx context.Context, r Receipt) error {
	now := time.Now()
	r.ID, r.Claim, r.Attempts, r.CreatedAt, r.UpdatedAt = uuid.New(), nil, 1, now, now
	if err := g.db.WithContext(ctx).Create(&r).Error; err != nil {
		return err
	}
	if r.Status == StatusRejected {
		return g.pruneRejected(ctx, r.OrgID)
	}
	return nil
}

func (g *GormReceiptStore) pruneRejected(ctx context.Context, org uuid.UUID) error {
	max := g.MaxRejected
	if max <= 0 {
		max = DefaultMaxRejected
	}
	var n int64
	q := g.db.WithContext(ctx).Model(&Receipt{}).Where("org_id = ? AND status = ?", org, StatusRejected)
	if err := q.Count(&n).Error; err != nil || n <= int64(max) {
		return err
	}
	var ids []uuid.UUID
	if err := g.db.WithContext(ctx).Model(&Receipt{}).Where("org_id = ? AND status = ?", org, StatusRejected).
		Order("created_at ASC").Limit(int(n)-max).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return g.db.WithContext(ctx).Where("id IN ?", ids).Delete(&Receipt{}).Error
}

// FinishAttempt is Finish fenced by the owning attempt number: a slow worker
// whose lease expired cannot overwrite the worker that re-claimed the event.
func (g *GormReceiptStore) FinishAttempt(ctx context.Context, id uuid.UUID, attempt int, status, recordID, errCode string) (bool, error) {
	res := g.db.WithContext(ctx).Model(&Receipt{}).
		Where("id = ? AND attempts = ? AND status = ?", id, attempt, StatusProcessing).
		Updates(map[string]any{"status": status, "record_id": recordID, "error_code": errCode, "updated_at": time.Now()})
	return res.RowsAffected == 1, res.Error
}

func (g *GormReceiptStore) List(ctx context.Context, org uuid.UUID, f ReceiptFilter) ([]Receipt, error) {
	if f.EventKey != "" && f.Contract == "" {
		return nil, ErrEventKeyNeedsContract
	}
	q := g.db.WithContext(ctx).Where("org_id = ?", org)
	if f.Contract != "" {
		q = q.Where("contract = ?", f.Contract)
		if f.EventKey != "" {
			q = q.Where("key_hash IN ?", keyHashes(org, f.Contract, f.EventKey, f.Shadow))
		}
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Shadow != nil {
		q = q.Where("shadow = ?", *f.Shadow)
	}
	lim := f.Limit
	if lim <= 0 || lim > 500 {
		lim = 100
	}
	var out []Receipt
	err := q.Order("created_at DESC").Limit(lim).Find(&out).Error
	return out, err
}

// hasDone reports whether any action of the chain already took effect.
func hasDone(steps []ActionStep) bool {
	for _, s := range steps {
		if s.Status == StepDone || s.Status == StepError {
			return true
		}
	}
	return false
}

// resumeBlocked: a chain with applied actions can only resume with the SAME
// body; another body under the same key is an idempotency conflict.
func resumeBlocked(cur Receipt, bodyHash string) bool {
	return hasDone(cur.Progress) && cur.BodyHash != bodyHash
}

// FailedAction is the index of the action that stopped the chain.
func (r Receipt) FailedAction() (int, bool) {
	for _, s := range r.Progress {
		if s.Status == StepFailed {
			return s.Index, true
		}
	}
	return 0, false
}

// SaveProgress is fenced by the owning attempt: it writes (and renews the
// lease) only while the receipt is still processing under `attempt`. owned=false
// means another worker re-claimed the event; the caller must stop applying
// effects.
func (m *MemoryReceiptStore) SaveProgress(_ context.Context, id uuid.UUID, attempt int, steps []ActionStep) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return false, errors.New("receipt not found")
	}
	if r.Attempts != attempt || r.Status != StatusProcessing {
		return false, nil
	}
	r.Progress, r.UpdatedAt = append([]ActionStep(nil), steps...), m.now()
	return true, nil
}

func (g *GormReceiptStore) SaveProgress(ctx context.Context, id uuid.UUID, attempt int, steps []ActionStep) (bool, error) {
	res := g.db.WithContext(ctx).Model(&Receipt{}).
		Where("id = ? AND attempts = ? AND status = ?", id, attempt, StatusProcessing).
		Select("Progress", "UpdatedAt").Updates(&Receipt{Progress: steps, UpdatedAt: time.Now()})
	return res.RowsAffected == 1, res.Error
}
