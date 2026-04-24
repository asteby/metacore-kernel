package notifications

import (
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// Status values for QueueEntry.Status.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusSent       = "sent"
	StatusFailed     = "failed"
)

// QueueEntry stores pending and completed notification deliveries.
// Provides auditing, retry on failure, and deduplication.
//
// SourceType / SourceID let consumers correlate an entry back to whatever
// triggered it (a tool, a workflow, a webhook, …) without leaking
// app-specific fields into the kernel.  Both are optional.
type QueueEntry struct {
	// BaseUUIDModel supplies ID, OrganizationID (tenant scope), CreatedByID,
	// CreatedAt, UpdatedAt, and DeletedAt (soft-delete).  Queries in Service
	// scope by OrganizationID from this embedded field.
	modelbase.BaseUUIDModel

	// Source: opaque identifiers for the trigger.  E.g. SourceType="agent_tool"
	// + SourceID = tool UUID, or SourceType="webhook" + SourceID = event ID.
	SourceType string     `gorm:"size:64;index" json:"source_type,omitempty"`
	SourceID   *uuid.UUID `gorm:"type:uuid;index" json:"source_id,omitempty"`
	SourceName string     `gorm:"size:255" json:"source_name,omitempty"`

	// Event: domain event name (status_changed, invoice_paid, etc.).  Used in
	// dedup hashing and surfaced to ChannelHandlers via QueueEntry.Event.
	Event string `gorm:"size:100;index;not null" json:"event"`

	// Channel: handler key.  Apps register handlers by channel name; the
	// worker dispatches Deliver to the handler that matches.
	Channel string `gorm:"size:50;not null" json:"channel"`

	// Target: free-form delivery destination interpreted by the handler
	// (phone number, email, URL, internal user ID, …).
	Target string `gorm:"size:255" json:"target"`

	// Message is the rendered payload delivered as-is to the channel.
	Message string `gorm:"type:text;not null" json:"message"`

	// ContextRef is an opaque app-defined reference (conversation ID, ticket
	// ID, …) the handler may use to fetch additional context.  Stored as
	// string so apps can use UUID, ULID, slug or whatever they need.
	ContextRef string `gorm:"size:128;index" json:"context_ref,omitempty"`

	// HandlerHint is an optional handler-specific hint (device ID, transport
	// override, …).  Treated as opaque by the kernel.
	HandlerHint string `gorm:"size:128" json:"handler_hint,omitempty"`

	// Delivery status & retry bookkeeping.
	Status     string     `gorm:"size:20;default:'pending';index" json:"status"`
	Attempts   int        `gorm:"default:0" json:"attempts"`
	MaxRetries int        `gorm:"default:3" json:"max_retries"`
	Error      string     `gorm:"type:text" json:"error,omitempty"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
	NextRetry  *time.Time `gorm:"index" json:"next_retry,omitempty"`

	// DedupKey: hash of event|channel|target|message (or supplied by caller).
	// Within Service.DedupWindow another enqueue with the same key + org
	// is silently dropped.
	DedupKey string `gorm:"size:64;index" json:"dedup_key,omitempty"`
}

// TableName pins the table to a stable name regardless of pluralization.
func (QueueEntry) TableName() string { return "notification_queue_entries" }
