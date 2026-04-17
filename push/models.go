package push

import (
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// PushSubscription stores a browser's Push subscription.
type PushSubscription struct {
	modelbase.BaseUUIDModel
	UserID     uuid.UUID  `gorm:"type:uuid;index" json:"user_id"`
	Endpoint   string     `gorm:"size:500;uniqueIndex" json:"endpoint"`
	P256DH     string     `gorm:"size:255" json:"p256dh"`
	Auth       string     `gorm:"size:255" json:"auth"`
	DeviceType string     `gorm:"size:20" json:"device_type"`
	UserAgent  string     `gorm:"size:500" json:"user_agent,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (PushSubscription) TableName() string { return "push_subscriptions" }
