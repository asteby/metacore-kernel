// Package gormledger is the SQL batch.Ledger: one row per processed manifest
// in dataload_batches, scoped by (organization_id, kind). It has the same
// shape as the intel_import_batches tables hosts already ran (external_key is
// the manifest sha256, file_sha256 the content print), plus kind, so a host
// with its own table can keep it behind a small adapter instead.
package gormledger

import (
	"context"
	"time"

	"github.com/asteby/metacore-kernel/dataload"
	"github.com/asteby/metacore-kernel/dataload/batch"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TableName is the table the ledger owns.
const TableName = "dataload_batches"

// Row is the persisted form of a batch.Record.
type Row struct {
	ID             uint      `gorm:"primaryKey"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:ux_dataload_batches_key,priority:1;index:ix_dataload_batches_last,priority:1"`
	Kind           string    `gorm:"size:64;not null;uniqueIndex:ux_dataload_batches_key,priority:2;index:ix_dataload_batches_last,priority:2"`
	// ExternalKey is the manifest sha256.
	ExternalKey string `gorm:"size:64;not null;uniqueIndex:ux_dataload_batches_key,priority:3"`
	// FileSHA256 is the content print (see batch.ContentPrint).
	FileSHA256  string `gorm:"size:64;not null"`
	GeneratedAt *time.Time
	SameContent bool               `gorm:"not null;default:false"`
	Files       []string           `gorm:"serializer:json"`
	Counts      map[string]int     `gorm:"serializer:json"`
	Warnings    []dataload.Message `gorm:"serializer:json"`
	CreatedAt   time.Time          `gorm:"not null;index:ix_dataload_batches_last,priority:3"`
}

// TableName implements gorm's Tabler.
func (Row) TableName() string { return TableName }

// Migrate creates dataload_batches (AutoMigrate; idempotent). Hosts that own
// their schema through their own migrations can skip it and create the table
// from Row.
func Migrate(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).AutoMigrate(&Row{})
}

// Ledger implements batch.Ledger over GORM.
type Ledger struct{ db *gorm.DB }

// New returns a Ledger; call Migrate first (or own the schema).
func New(db *gorm.DB) *Ledger { return &Ledger{db: db} }

var _ batch.Ledger = (*Ledger)(nil)

func (l *Ledger) Done(ctx context.Context, s batch.Scope, manifestSHA string) (bool, error) {
	var n int64
	err := l.db.WithContext(ctx).Model(&Row{}).
		Where("organization_id = ? AND kind = ? AND external_key = ?", s.OrgID, s.Kind, manifestSHA).
		Limit(1).Count(&n).Error
	return n > 0, err
}

func (l *Ledger) LastContentPrint(ctx context.Context, s batch.Scope) (string, error) {
	var rows []Row
	err := l.db.WithContext(ctx).
		Where("organization_id = ? AND kind = ?", s.OrgID, s.Kind).
		Order("created_at DESC, id DESC").Limit(1).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].FileSHA256, nil
}

// Record inserts the batch; a manifest already recorded is left as it is.
func (l *Ledger) Record(ctx context.Context, s batch.Scope, rec batch.Record) error {
	row := Row{
		OrganizationID: s.OrgID, Kind: s.Kind, ExternalKey: rec.ManifestSHA, FileSHA256: rec.ContentPrint,
		GeneratedAt: rec.GeneratedAt, SameContent: rec.SameContent, Files: rec.Files, Counts: rec.Counts,
		Warnings: rec.Warnings, CreatedAt: time.Now().UTC(),
	}
	return l.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "organization_id"}, {Name: "kind"}, {Name: "external_key"}},
		DoNothing: true,
	}).Create(&row).Error
}
