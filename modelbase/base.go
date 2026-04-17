package modelbase

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ContextKeyCreatedByUserID is the gorm.DB context key that, if set to a
// non-nil *uuid.UUID, will be copied into BaseUUIDModel.CreatedByID by the
// BeforeCreate hook. This mirrors the ops backend convention.
const ContextKeyCreatedByUserID = "activity_log:user_id"

// BaseUUIDModel provides common fields for every tenant-scoped record in the
// platform: a UUID primary key, an organization scope, audit timestamps, a
// soft-delete tombstone and a created-by foreign key.
//
// Downstream models embed this struct:
//
//	type Contact struct {
//	    modelbase.BaseUUIDModel
//	    Name string
//	}
//
// OrganizationID lives here (not in the embedder) because tenant scoping is a
// cross-cutting concern enforced by middleware, query scopes, and indexes on
// every table.
type BaseUUIDModel struct {
	ID             uuid.UUID      `json:"id" gorm:"type:uuid;primary_key;default:gen_random_uuid()"`
	OrganizationID uuid.UUID      `json:"organization_id" gorm:"type:uuid;index"`
	CreatedByID    *uuid.UUID     `json:"created_by_id,omitempty" gorm:"type:uuid;index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`
}

// BeforeCreate fills the primary key if absent and copies the user id stashed
// in the gorm.DB context (if any) into CreatedByID so audit trails are
// populated transparently.
func (b *BaseUUIDModel) BeforeCreate(tx *gorm.DB) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	if b.CreatedByID == nil {
		if val, ok := tx.Get(ContextKeyCreatedByUserID); ok {
			if uid, ok := val.(*uuid.UUID); ok && uid != nil {
				b.CreatedByID = uid
			}
		}
	}
	return nil
}
