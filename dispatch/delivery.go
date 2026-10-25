package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Delivery status values. A delivery starts `pending`, becomes `delivered` on
// the first successful invocation, or `dead` once it exhausts MaxAttempts (or
// the guest says the failure is not retryable). `cancelled` is terminal too:
// the subscription disappeared (addon uninstalled/disabled) before it ran.
//
// A `pending` row is the durable work item: it carries the payload,
// `next_attempt_at` and a lease, so ANY process can pick it up after a restart.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusDead      = "dead"
	StatusCancelled = "cancelled"
)

// Delivery is one row of the dispatch ledger: the record of (attempting to)
// deliver one bus event to one addon subscription. It is the idempotency key
// AND the dead-letter queue.
//
// DeliveryID is the deterministic dedup key — hash(eventID + subscription
// identity). The first time an event matches a subscription we INSERT the row;
// a duplicate publish of the same event (same source mutation re-fanned) hits
// the unique index and is a no-op. This makes re-delivery safe from day one
// without a separate inbox table.
type Delivery struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	DeliveryID     string     `gorm:"uniqueIndex;size:64;not null" json:"delivery_id"`
	OrganizationID uuid.UUID  `gorm:"type:uuid;index" json:"organization_id"`
	Event          string     `gorm:"size:512;index" json:"event"`
	AddonKey       string     `gorm:"size:256;index" json:"addon_key"`
	HandlerType    string     `gorm:"size:32" json:"handler_type"`
	Export         string     `gorm:"size:256" json:"export"`
	Status         string     `gorm:"size:16;index;not null" json:"status"`
	Attempts       int        `gorm:"not null" json:"attempts"`
	LastError      string     `gorm:"type:text" json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	// Emitter is the publisher the bus recorded for the event: an addon key
	// (guest event_emit), "kernel", or the owner addon of a canonical CRUD
	// event the host published. Empty on rows written before the column
	// existed (additive column, see Migrate).
	Emitter string `gorm:"size:256" json:"emitter,omitempty"`

	// --- durable queue columns (additive; see ledger.go) -------------------

	// Payload is the exact JSON delivered to the handler (envelope included).
	// Persisting it is what lets a restarted process resume a `pending`
	// delivery, and what lets Replay re-deliver it. Rows written before this
	// column existed have it empty and are left alone.
	Payload string `gorm:"type:text;not null;default:''" json:"-"`
	// SubEvent is the subscribed pattern (Subscription.Event); with
	// AddonKey/HandlerType/Export it identifies the subscription so a resumed
	// delivery re-resolves it (fresh Settings, still installed?).
	SubEvent string `gorm:"size:512;not null;default:''" json:"sub_event,omitempty"`
	// EventID is the envelope id; ActorID/BranchID/CorrelationID are re-attached
	// to the delivery ctx on resume.
	EventID       string `gorm:"size:128;not null;default:''" json:"event_id,omitempty"`
	ActorID       string `gorm:"size:64;not null;default:''" json:"-"`
	BranchID      string `gorm:"size:64;not null;default:''" json:"-"`
	CorrelationID string `gorm:"size:128;not null;default:''" json:"-"`
	// Generation is 0 for the original delivery and N for the Nth Replay;
	// RootDeliveryID is the delivery_id of generation 0 of the chain.
	Generation     int    `gorm:"not null;default:0" json:"generation"`
	RootDeliveryID string `gorm:"size:64;not null;default:''" json:"-"`
	// NextAttemptAt is when a `pending` row becomes claimable.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	// LeaseUntil/LeaseToken are the claim: while LeaseUntil is in the future
	// only the holder of LeaseToken may run or finish the row. A crashed
	// holder simply lets the lease expire.
	LeaseUntil *time.Time `json:"-"`
	LeaseToken string     `gorm:"size:64;not null;default:''" json:"-"`
	// NotReadySince is when the subscriber first answered "not ready" in the
	// current streak (waiting for a module to load does not spend attempts).
	NotReadySince *time.Time `json:"-"`
}

// deliveryID derives the deterministic idempotency key for one (event,
// subscription) pair. The subscription identity is (addonKey, event-pattern,
// handlerType, function) so the SAME source event delivered to two distinct
// subscriptions of the same addon (e.g. a wildcard + an exact match) yields
// two distinct ledger rows, while a re-publish of the same event to the same
// subscription collapses to one.
//
// occurrenceID identifies the PUBLICATION, not the mutated row: it is the
// canonical event's `occurrence_id` (a uuid stamped once per publish), or a
// fingerprint of the payload when the envelope predates that field. It used to
// be the mutated row id, which meant the first update of a row consumed the
// only delivery it would ever get and every later update was dropped as a
// phantom re-publish (#321). Do not reintroduce a row-scoped key here.
func deliveryID(eventName, occurrenceID string, sub Subscription) string {
	h := sha256.New()
	// Length-prefix each field so no concatenation collision is possible
	// across field boundaries (e.g. ("a","bc") vs ("ab","c")).
	for _, f := range []string{
		eventName, occurrenceID,
		sub.AddonKey, sub.Event, sub.HandlerType, sub.Function,
	} {
		_, _ = h.Write([]byte(f))
		_, _ = h.Write([]byte{0x00})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// replayDeliveryID derives the idempotency key of generation gen (>0) of a
// delivery chain. Generation 0 keeps the original deliveryID, so the ledger
// never collides with a Replay and a Replay never collides with itself.
func replayDeliveryID(root string, gen int) string {
	h := sha256.New()
	_, _ = h.Write([]byte(root))
	_, _ = h.Write([]byte{0x00})
	_, _ = h.Write([]byte("generation:" + strconv.Itoa(gen)))
	return hex.EncodeToString(h.Sum(nil))
}

// ForgetDeliveries deletes every ledger row for one addon+event so the next
// publish of the same occurrence is not a no-op. Use after a guest was
// wrongly marked delivered (`success:false` treated as ok).
func ForgetDeliveries(db *gorm.DB, tableName, addonKey, eventName string) (int64, error) {
	if db == nil || addonKey == "" || eventName == "" {
		return 0, nil
	}
	if tableName == "" {
		tableName = DefaultTableName
	}
	res := db.Table(tableName).Where("addon_key = ? AND event = ?", addonKey, eventName).Delete(&Delivery{})
	return res.RowsAffected, res.Error
}

// Migrate ensures the delivery ledger table exists with the Delivery schema.
// Safe to call repeatedly (GORM AutoMigrate is additive). Wire calls this
// automatically; a host that owns migration timing can call it explicitly and
// pass the already-migrated table to Wire.
func Migrate(db *gorm.DB, tableName string) error {
	if tableName == "" {
		tableName = DefaultTableName
	}
	if err := db.Table(tableName).AutoMigrate(&Delivery{}); err != nil {
		return err
	}
	// The sweeper's claim path only ever looks at pending rows; a partial index
	// keeps it O(pending) however many delivered rows the ledger has piled up.
	// (Both Postgres and SQLite support partial indexes.)
	idx := "idx_" + strings.NewReplacer(".", "_", `"`, "").Replace(tableName) + "_due"
	return db.Exec("CREATE INDEX IF NOT EXISTS " + idx + " ON " + tableName + " (next_attempt_at) WHERE status = 'pending'").Error
}
