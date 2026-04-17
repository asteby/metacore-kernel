package webhooks

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// StringSlice is a []string that persists as JSON / comma-separated text.
// It works on both Postgres (jsonb) and SQLite (TEXT).
type StringSlice []string

// Scan implements sql.Scanner.
func (s *StringSlice) Scan(v any) error {
	if v == nil {
		*s = nil
		return nil
	}
	var raw []byte
	switch x := v.(type) {
	case []byte:
		raw = x
	case string:
		raw = []byte(x)
	default:
		return errors.New("webhooks: StringSlice scan expects []byte or string")
	}
	if len(raw) == 0 {
		*s = nil
		return nil
	}
	// try JSON first
	if raw[0] == '[' {
		return json.Unmarshal(raw, s)
	}
	// fallback: comma-separated
	*s = strings.Split(string(raw), ",")
	return nil
}

// Value implements driver.Valuer.
func (s StringSlice) Value() (driver.Value, error) {
	if s == nil {
		return nil, nil
	}
	return json.Marshal(s)
}

// JSONMap is a generic JSON payload persisted as jsonb/TEXT.
type JSONMap map[string]any

func (m *JSONMap) Scan(v any) error {
	if v == nil {
		*m = nil
		return nil
	}
	var raw []byte
	switch x := v.(type) {
	case []byte:
		raw = x
	case string:
		raw = []byte(x)
	default:
		return errors.New("webhooks: JSONMap scan expects []byte or string")
	}
	if len(raw) == 0 {
		*m = nil
		return nil
	}
	return json.Unmarshal(raw, m)
}

func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// Webhook is an outbound webhook subscription.
type Webhook struct {
	modelbase.BaseUUIDModel
	Name       string      `gorm:"size:255" json:"name"`
	URL        string      `gorm:"size:1024" json:"url"`
	Events     StringSlice `gorm:"type:text" json:"events"`
	Secret     string      `gorm:"size:128" json:"-"`
	Active     bool        `gorm:"default:true" json:"active"`
	RetryMax   int         `gorm:"default:3" json:"retry_max"`
	TimeoutSec int         `gorm:"default:15" json:"timeout_sec"`

	// Owner polymorphism — the escape hatch that lets apps scope webhooks
	// to organizations, devices, users, or anything else without forking.
	OwnerType string    `gorm:"size:50;index" json:"owner_type"`
	OwnerID   uuid.UUID `gorm:"type:uuid;index" json:"owner_id"`

	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
	FailureCount    int        `json:"failure_count"`
	SuccessCount    int        `json:"success_count"`
}

func (Webhook) TableName() string { return "webhooks" }

// WebhookDelivery records each attempt at sending a webhook.
type WebhookDelivery struct {
	modelbase.BaseUUIDModel
	WebhookID       uuid.UUID  `gorm:"type:uuid;index" json:"webhook_id"`
	Event           string     `gorm:"size:100;index" json:"event"`
	Payload         JSONMap    `gorm:"type:text" json:"payload"`
	RequestHeaders  JSONMap    `gorm:"type:text" json:"request_headers,omitempty"`
	ResponseStatus  int        `json:"response_status"`
	ResponseBody    string     `gorm:"type:text" json:"response_body"`
	ResponseHeaders JSONMap    `gorm:"type:text" json:"response_headers,omitempty"`
	AttemptCount    int        `json:"attempt_count"`
	Succeeded       bool       `gorm:"index" json:"succeeded"`
	ErrorMessage    string     `gorm:"size:500" json:"error_message,omitempty"`
	DeliveredAt     *time.Time `json:"delivered_at,omitempty"`
	NextAttemptAt   *time.Time `gorm:"index" json:"next_attempt_at,omitempty"`
}

func (WebhookDelivery) TableName() string { return "webhook_deliveries" }
