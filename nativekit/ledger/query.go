package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/nativekit/dec"
)

// Balance reads one cell without locking. A cell that does not exist yet is
// zero. bucket "" means DefaultBucket.
func (l *Ledger) Balance(ctx context.Context, db *gorm.DB, org uuid.UUID, account, bucket string) (dec.D, error) {
	if bucket == "" {
		bucket = DefaultBucket
	}
	var c Cell
	err := db.WithContext(ctx).Table(l.tCells).
		Where("org_id = ? AND ledger = ? AND account = ? AND bucket = ?", org, l.name, account, bucket).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return c.Balance, err
}

// Balances reads every bucket of an account.
func (l *Ledger) Balances(ctx context.Context, db *gorm.DB, org uuid.UUID, account string) (map[string]dec.D, error) {
	var cells []Cell
	if err := db.WithContext(ctx).Table(l.tCells).
		Where("org_id = ? AND ledger = ? AND account = ?", org, l.name, account).Find(&cells).Error; err != nil {
		return nil, err
	}
	out := make(map[string]dec.D, len(cells))
	for _, c := range cells {
		out[c.Bucket] = c.Balance
	}
	return out, nil
}

// HistoryFilter selects entries of one cell, newest first.
type HistoryFilter struct {
	Account string
	Bucket  string // "" = DefaultBucket
	Before  time.Time
	Limit   int // 0 = 50, capped at 500
}

// History lists a cell's entries, newest first, each with the balance that
// resulted from it.
func (l *Ledger) History(ctx context.Context, db *gorm.DB, org uuid.UUID, f HistoryFilter) ([]Entry, error) {
	if f.Bucket == "" {
		f.Bucket = DefaultBucket
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	q := db.WithContext(ctx).Table(l.tEntries).
		Where("org_id = ? AND ledger = ? AND account = ? AND bucket = ?", org, l.name, f.Account, f.Bucket)
	if !f.Before.IsZero() {
		q = q.Where("created_at < ?", f.Before)
	}
	var out []Entry
	err := q.Order("created_at DESC, seq DESC").Limit(f.Limit).Find(&out).Error
	return out, err
}

// Drift is a cell whose stored balance disagrees with the sum of its entries.
type Drift struct {
	Account     string
	Bucket      string
	Stored      dec.D
	FromEntries dec.D
}

// Reconcile compares every cell of the ledger with the sum of its entries and
// returns the disagreements (an empty slice means the books are consistent). A
// cell that predates the ledger (imported opening balance) drifts by design:
// import it as an "opening" posting instead of writing the cell directly.
func (l *Ledger) Reconcile(ctx context.Context, db *gorm.DB, org uuid.UUID) ([]Drift, error) {
	var cells []Cell
	if err := db.WithContext(ctx).Table(l.tCells).Where("org_id = ? AND ledger = ?", org, l.name).Find(&cells).Error; err != nil {
		return nil, err
	}
	var sums []struct {
		Account string
		Bucket  string
		Total   dec.D
	}
	if err := db.WithContext(ctx).Table(l.tEntries).
		Select("account, bucket, SUM(delta) AS total").
		Where("org_id = ? AND ledger = ?", org, l.name).
		Group("account, bucket").Scan(&sums).Error; err != nil {
		return nil, err
	}
	want := make(map[cellKey]dec.D, len(sums))
	for _, s := range sums {
		want[cellKey{s.Account, s.Bucket}] = s.Total
	}
	var out []Drift
	seen := make(map[cellKey]bool, len(cells))
	for _, c := range cells {
		k := cellKey{c.Account, c.Bucket}
		seen[k] = true
		if got := want[k]; got != c.Balance {
			out = append(out, Drift{Account: c.Account, Bucket: c.Bucket, Stored: c.Balance, FromEntries: got})
		}
	}
	for k, total := range want { // entries without a cell: the cell row went missing
		if !seen[k] && total != 0 {
			out = append(out, Drift{Account: k.account, Bucket: k.bucket, FromEntries: total})
		}
	}
	return out, nil
}
