package audit

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DefaultTableName is the table the audit Recorder writes to and the Query
// service reads from. It can be overridden per-Wire / per-Query via
// WithTableName so an app can keep its historical table name (e.g. ops keeps
// "activity_logs" for backward compatibility).
const DefaultTableName = "audit_entries"

// Entry is one row of the audit trail: a single CRUD mutation captured off the
// kernel events.Bus. It is intentionally a flat, app-agnostic shape — the
// kernel does NOT denormalise actor name/email (it does not know the host's
// users table). Only ActorID is stored; an optional ActorResolver (see
// WithActorResolver) lets a host backfill name/email if it wants them.
//
// Before/After/Changes are stored as JSONB strings (pointers, nil when absent):
//   - created → After only
//   - updated → Before + After + Changes (field-level diff)
//   - deleted → Before only
//
// The table name is pinned at runtime (Recorder.tableName / Query params), not
// via a GORM TableName() method, so a single struct serves multiple tables.
type Entry struct {
	ID             uuid.UUID  `json:"id" gorm:"type:uuid;primary_key;default:gen_random_uuid()"`
	OrganizationID uuid.UUID  `json:"organization_id" gorm:"type:uuid;index"`
	CorrelationID  *uuid.UUID `json:"correlation_id,omitempty" gorm:"type:uuid;index"`
	ActorID        *uuid.UUID `json:"actor_id,omitempty" gorm:"type:uuid;index"`
	AddonKey       string     `json:"addon_key" gorm:"size:100"`
	Model          string     `json:"model" gorm:"column:model;size:255;index"`
	RecordID       string     `json:"record_id" gorm:"size:255;index"`
	Action         string     `json:"action" gorm:"size:50;index"` // created|updated|deleted
	Before         *string    `json:"before,omitempty" gorm:"type:jsonb"`
	After          *string    `json:"after,omitempty" gorm:"type:jsonb"`
	Changes        *string    `json:"changes,omitempty" gorm:"type:jsonb"` // field-level diff (updates)
	OccurredAt     time.Time  `json:"occurred_at" gorm:"index"`

	// ActorName / ActorEmail are NOT persisted by the kernel. They are populated
	// in-memory by an optional ActorResolver (Recorder write-side) or left empty.
	// gorm:"-" keeps them off the table; json omitempty keeps them off the wire
	// when blank.
	ActorName  string `json:"actor_name,omitempty" gorm:"-"`
	ActorEmail string `json:"actor_email,omitempty" gorm:"-"`
}

// BeforeCreate fills the primary key and OccurredAt if the caller left them
// zero, so a host can construct an Entry with just the business fields.
func (e *Entry) BeforeCreate(_ *gorm.DB) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	return nil
}
