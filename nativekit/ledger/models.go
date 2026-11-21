package ledger

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/nativekit/dec"
)

// Cell is one balance: the running total of one bucket of one account in one
// ledger. A "stock" ledger has a cell per (product@warehouse, quantity) and
// (product@warehouse, reserved); a wallet has one per customer; a cash drawer
// one per register and tender. Account and bucket are opaque strings the module
// chooses (use nativekit.Key to compose them).
type Cell struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrgID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:ux_ledger_cell,priority:1"`
	Ledger    string    `gorm:"size:120;not null;uniqueIndex:ux_ledger_cell,priority:2"`
	Account   string    `gorm:"size:250;not null;uniqueIndex:ux_ledger_cell,priority:3"`
	Bucket    string    `gorm:"size:60;not null;uniqueIndex:ux_ledger_cell,priority:4"`
	Balance   dec.D     `gorm:"type:numeric(24,6);not null;default:0"`
	Version   int64     `gorm:"not null;default:0"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName is the shared cells table (every ledger is a row-set in it, keyed
// by Ledger).
func (Cell) TableName() string { return "metacore_ledger_cells" }

// Posting is one atomic, balanced-by-construction group of entries: the unit of
// idempotency, reversal and eventing. A sale of three lines is one posting.
type Posting struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey"`
	OrgID       uuid.UUID  `gorm:"type:uuid;not null;index:ix_ledger_posting_ref,priority:1;uniqueIndex:ux_ledger_posting_key,priority:1"`
	Ledger      string     `gorm:"size:120;not null;index:ix_ledger_posting_ref,priority:2;uniqueIndex:ux_ledger_posting_key,priority:2"`
	IdemKey     *string    `gorm:"size:250;uniqueIndex:ux_ledger_posting_key,priority:3"`
	Fingerprint string     `gorm:"size:64"`
	Kind        string     `gorm:"size:60;not null"`
	RefKind     string     `gorm:"size:60;index:ix_ledger_posting_ref,priority:3"`
	RefID       string     `gorm:"size:120;index:ix_ledger_posting_ref,priority:4"`
	Actor       string     `gorm:"size:120"`
	Memo        string     `gorm:"type:text"`
	Meta        string     `gorm:"type:text"`
	ReversesID  *uuid.UUID `gorm:"type:uuid;uniqueIndex"`
	ReversedBy  *uuid.UUID `gorm:"type:uuid"`
	OccurredAt  time.Time  `gorm:"not null"`
	CreatedAt   time.Time
}

// TableName is the postings table.
func (Posting) TableName() string { return "metacore_ledger_postings" }

// Entry is one immutable row of the ledger: the delta applied to one cell and
// the balance that resulted. Entries are append-only; a mistake is corrected
// with a reversal posting, never an UPDATE.
type Entry struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	PostingID    uuid.UUID `gorm:"type:uuid;not null;index"`
	OrgID        uuid.UUID `gorm:"type:uuid;not null;index:ix_ledger_entry_cell,priority:1"`
	Ledger       string    `gorm:"size:120;not null;index:ix_ledger_entry_cell,priority:2"`
	Account      string    `gorm:"size:250;not null;index:ix_ledger_entry_cell,priority:3"`
	Bucket       string    `gorm:"size:60;not null;index:ix_ledger_entry_cell,priority:4"`
	Seq          int       `gorm:"not null"`
	Delta        dec.D     `gorm:"type:numeric(24,6);not null"`
	BalanceAfter dec.D     `gorm:"type:numeric(24,6);not null"`
	CreatedAt    time.Time `gorm:"index:ix_ledger_entry_cell,priority:5"`
}

// TableName is the entries table.
func (Entry) TableName() string { return "metacore_ledger_entries" }

// Migrate creates the ledger tables and indexes. Safe to call at every boot.
func Migrate(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).AutoMigrate(&Cell{}, &Posting{}, &Entry{})
}
