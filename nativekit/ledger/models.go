package ledger

import (
	"context"
	"regexp"
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

var prefixRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func tableNames(prefix string) (cells, postings, entries string) {
	if prefix == "" {
		return Cell{}.TableName(), Posting{}.TableName(), Entry{}.TableName()
	}
	if !prefixRe.MatchString(prefix) {
		panic("ledger: invalid TablePrefix " + prefix)
	}
	return prefix + "cells", prefix + "postings", prefix + "entries"
}

// Migrate creates this ledger's tables. With the default (shared) tables it is
// the package-level Migrate; with Config.TablePrefix it creates the prefixed
// tables and indexes (index names carry the prefix, since they are
// schema-global). Safe to call at every boot.
func (l *Ledger) Migrate(ctx context.Context, db *gorm.DB) error {
	if l.cfg.TablePrefix == "" {
		return Migrate(ctx, db)
	}
	c, p, e := l.tCells, l.tPostings, l.tEntries
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + c + ` (
			id uuid PRIMARY KEY, org_id uuid NOT NULL, ledger varchar(120) NOT NULL, account varchar(250) NOT NULL,
			bucket varchar(60) NOT NULL, balance numeric(24,6) NOT NULL DEFAULT 0, version bigint NOT NULL DEFAULT 0,
			created_at timestamp, updated_at timestamp)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_` + c + ` ON ` + c + ` (org_id, ledger, account, bucket)`,
		`CREATE TABLE IF NOT EXISTS ` + p + ` (
			id uuid PRIMARY KEY, org_id uuid NOT NULL, ledger varchar(120) NOT NULL, idem_key varchar(250), fingerprint varchar(64),
			kind varchar(60) NOT NULL, ref_kind varchar(60), ref_id varchar(120), actor varchar(120), memo text, meta text,
			reverses_id uuid, reversed_by uuid, occurred_at timestamp NOT NULL, created_at timestamp)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_` + p + `_key ON ` + p + ` (org_id, ledger, idem_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_` + p + `_rev ON ` + p + ` (reverses_id)`,
		`CREATE INDEX IF NOT EXISTS ix_` + p + `_ref ON ` + p + ` (org_id, ledger, ref_kind, ref_id)`,
		`CREATE TABLE IF NOT EXISTS ` + e + ` (
			id uuid PRIMARY KEY, posting_id uuid NOT NULL, org_id uuid NOT NULL, ledger varchar(120) NOT NULL,
			account varchar(250) NOT NULL, bucket varchar(60) NOT NULL, seq integer NOT NULL,
			delta numeric(24,6) NOT NULL, balance_after numeric(24,6) NOT NULL, created_at timestamp)`,
		`CREATE INDEX IF NOT EXISTS ix_` + e + `_posting ON ` + e + ` (posting_id)`,
		`CREATE INDEX IF NOT EXISTS ix_` + e + `_cell ON ` + e + ` (org_id, ledger, account, bucket, created_at)`,
	}
	for _, s := range stmts {
		if err := db.WithContext(ctx).Exec(s).Error; err != nil {
			return err
		}
	}
	return nil
}
