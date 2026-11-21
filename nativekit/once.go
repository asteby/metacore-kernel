package nativekit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OpKey is the durable idempotency record of one native operation. The primary
// key (org, scope, key) is the unique index that makes concurrent duplicates
// safe: the database, not application code, decides who applies the operation.
type OpKey struct {
	OrgID       uuid.UUID `gorm:"type:uuid;primaryKey"`
	Scope       string    `gorm:"primaryKey;size:120"`
	Key         string    `gorm:"column:idem_key;primaryKey;size:250"`
	Fingerprint string    `gorm:"size:64"`
	Done        bool      `gorm:"not null;default:false"`
	Result      string    `gorm:"type:text"`
	CreatedAt   time.Time `gorm:"index"`
}

// TableName is the table backing Once.
func (OpKey) TableName() string { return "metacore_native_ops" }

// Migrate creates the idempotency table. Safe to call at every boot.
func Migrate(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).AutoMigrate(&OpKey{})
}

// OnceOption tunes Once.
type OnceOption func(*onceCfg)

type onceCfg struct{ fingerprint string }

// WithFingerprint binds the key to a payload digest (see Fingerprint). Reusing
// the key with a different digest fails with CodeIdempotencyConflict instead of
// replaying a response that belongs to another request.
func WithFingerprint(fp string) OnceOption { return func(c *onceCfg) { c.fingerprint = fp } }

// Once runs fn at most once per (org, scope, key), inside the caller's
// transaction tx, and replays the stored result on every later call.
//
//   - The claim row is inserted in tx, so it commits or rolls back together
//     with fn's writes: a failed operation leaves no key behind and can be
//     retried; a committed one can never run again.
//   - Two concurrent calls with the same key serialize on the unique index
//     (Postgres makes the second INSERT wait for the first transaction); the
//     second one then replays.
//   - An empty key disables idempotency: fn simply runs.
//   - T must round-trip through encoding/json; that is what is stored and
//     replayed.
//
// replayed is true when the result came from a previous run.
func Once[T any](tx *gorm.DB, org uuid.UUID, scope, key string, fn func() (T, error), opts ...OnceOption) (res T, replayed bool, err error) {
	if key == "" {
		res, err = fn()
		return res, false, err
	}
	var cfg onceCfg
	for _, o := range opts {
		o(&cfg)
	}
	claim := OpKey{OrgID: org, Scope: scope, Key: key, Fingerprint: cfg.fingerprint, CreatedAt: time.Now().UTC()}
	r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
	if r.Error != nil {
		return res, false, r.Error
	}
	if r.RowsAffected == 0 {
		return replay[T](tx, org, scope, key, cfg.fingerprint)
	}

	// We own the key. Run fn under a savepoint so a failure undoes its partial
	// writes and releases the claim even if the caller keeps using tx.
	ferr := tx.Transaction(func(*gorm.DB) error {
		var e error
		res, e = fn()
		return e
	})
	if ferr != nil {
		tx.Where("org_id = ? AND scope = ? AND idem_key = ?", org, scope, key).Delete(&OpKey{})
		var zero T
		return zero, false, ferr
	}
	b, merr := json.Marshal(res)
	if merr != nil {
		return res, false, merr
	}
	err = tx.Model(&OpKey{}).Where("org_id = ? AND scope = ? AND idem_key = ?", org, scope, key).
		Updates(map[string]any{"done": true, "result": string(b)}).Error
	return res, false, err
}

func replay[T any](tx *gorm.DB, org uuid.UUID, scope, key, fp string) (res T, replayed bool, err error) {
	var row OpKey
	if err = tx.Where("org_id = ? AND scope = ? AND idem_key = ?", org, scope, key).Take(&row).Error; err != nil {
		return res, false, err
	}
	if fp != "" && row.Fingerprint != "" && fp != row.Fingerprint {
		return res, false, E(CodeIdempotencyConflict, "idempotency key reused with a different payload", map[string]any{"scope": scope})
	}
	if !row.Done {
		// Same transaction claimed the key and has not finished: a duplicate
		// inside one operation, which is a caller bug rather than a retry.
		return res, false, E(CodeIdempotencyInFlight, "operation with this key is still running", map[string]any{"scope": scope})
	}
	if err = json.Unmarshal([]byte(row.Result), &res); err != nil {
		return res, false, err
	}
	return res, true, nil
}

// Cleanup deletes idempotency records older than olderThan. Keys only need to
// outlive the longest client retry window; a week is typical.
func Cleanup(ctx context.Context, db *gorm.DB, olderThan time.Duration) (int64, error) {
	r := db.WithContext(ctx).Where("created_at < ?", time.Now().UTC().Add(-olderThan)).Delete(&OpKey{})
	return r.RowsAffected, r.Error
}
