package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Delivery status values. A delivery starts `pending`, becomes `delivered` on
// the first successful invocation, or `dead` once it exhausts MaxAttempts.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusDead      = "dead"
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
}

// deliveryID derives the deterministic idempotency key for one (event,
// subscription) pair. The subscription identity is (addonKey, event-pattern,
// handlerType, function) so the SAME source event delivered to two distinct
// subscriptions of the same addon (e.g. a wildcard + an exact match) yields
// two distinct ledger rows, while a re-publish of the same event to the same
// subscription collapses to one.
//
// eventID is the CanonicalEvent.ID (the mutated row id) combined with the fired
// event name — together they identify the concrete event occurrence. (The bus
// does not carry a per-publish uuid; (event-name, row-id) is the stable
// natural key the canonical contract already provides.)
func deliveryID(eventName, eventRowID string, sub Subscription) string {
	h := sha256.New()
	// Length-prefix each field so no concatenation collision is possible
	// across field boundaries (e.g. ("a","bc") vs ("ab","c")).
	for _, f := range []string{
		eventName, eventRowID,
		sub.AddonKey, sub.Event, sub.HandlerType, sub.Function,
	} {
		_, _ = h.Write([]byte(f))
		_, _ = h.Write([]byte{0x00})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Migrate ensures the delivery ledger table exists with the Delivery schema.
// Safe to call repeatedly (GORM AutoMigrate is additive). Wire calls this
// automatically; a host that owns migration timing can call it explicitly and
// pass the already-migrated table to Wire.
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

// ForgetOccurrences deletes only the ledger rows whose DeliveryID matches
// hash(event, occurrence, subscription) for the given subscriptions.
func ForgetOccurrences(db *gorm.DB, tableName, eventName string, occurrenceIDs []string, subs []Subscription) (int64, error) {
	if db == nil || eventName == "" || len(occurrenceIDs) == 0 || len(subs) == 0 {
		return 0, nil
	}
	if tableName == "" {
		tableName = DefaultTableName
	}
	ids := make([]string, 0, len(occurrenceIDs)*len(subs))
	for _, occ := range occurrenceIDs {
		for _, sub := range subs {
			ids = append(ids, deliveryID(eventName, occ, sub))
		}
	}
	res := db.Table(tableName).Where("delivery_id IN ?", ids).Delete(&Delivery{})
	return res.RowsAffected, res.Error
}

func Migrate(db *gorm.DB, tableName string) error {
	if tableName == "" {
		tableName = DefaultTableName
	}
	return db.Table(tableName).AutoMigrate(&Delivery{})
}
