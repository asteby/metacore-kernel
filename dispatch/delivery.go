package dispatch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
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

// Migrate ensures the delivery ledger table and its sweeper index exist with the
// Delivery schema. Safe to call repeatedly (GORM AutoMigrate is additive). Wire
// calls the table half automatically (and builds the index off the startup
// path on Postgres); a host that owns migration timing can call Migrate
// explicitly and pass the already-migrated table to Wire.
func Migrate(db *gorm.DB, tableName string) error {
	if tableName == "" {
		tableName = DefaultTableName
	}
	if err := migrateTable(db, tableName); err != nil {
		return err
	}
	if err := ensureDueIndex(db, tableName); err != nil && !errors.Is(err, errDueIndexBusy) {
		return err
	}
	return nil
}

func migrateTable(db *gorm.DB, tableName string) error {
	return db.Table(tableName).AutoMigrate(&Delivery{})
}

const dueIndexCols = " (next_attempt_at) WHERE status = 'pending'"

// dueIndexName is the sweeper index of a ledger table.
func dueIndexName(tableName string) string {
	return "idx_" + strings.NewReplacer(".", "_", `"`, "").Replace(tableName) + "_due"
}

// dueIndexPlan decides which statements bring the sweeper index to a valid
// state. The sweeper's claim path only ever looks at pending rows; a partial
// index keeps it O(pending) however many delivered rows the ledger has piled up.
//
//   - Postgres builds it CONCURRENTLY (a plain CREATE INDEX takes a SHARE lock
//     that blocks every INSERT into the ledger — i.e. every event — for the
//     whole build on a big table). CONCURRENTLY cannot run inside a
//     transaction, so inTx falls back to the plain form. A build interrupted
//     midway leaves an INVALID index that IF NOT EXISTS would skip forever, so
//     an invalid one is dropped and recreated.
//   - SQLite keeps the plain statement.
//
// exists/valid describe the current index (ignored off Postgres).
func dueIndexPlan(dialect, tableName string, inTx, exists, valid bool) []string {
	idx := dueIndexName(tableName)
	if dialect != "postgres" || inTx {
		return []string{"CREATE INDEX IF NOT EXISTS " + idx + " ON " + tableName + dueIndexCols}
	}
	var out []string
	if exists && !valid {
		out = append(out, "DROP INDEX CONCURRENTLY IF EXISTS "+pgIndexRef(tableName, idx))
		exists = false
	}
	if !exists {
		out = append(out, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "+idx+" ON "+tableName+dueIndexCols)
	}
	return out
}

// pgIndexRef qualifies idx with the schema of a schema-qualified table name
// (an index always lives in its table's schema).
func pgIndexRef(tableName, idx string) string {
	if i := strings.LastIndex(tableName, "."); i > 0 {
		return tableName[:i] + "." + idx
	}
	return idx
}

func dbInTx(db *gorm.DB) bool {
	if db == nil || db.Statement == nil || db.Statement.ConnPool == nil {
		return false
	}
	_, ok := db.Statement.ConnPool.(gorm.TxCommitter)
	return ok
}

// errDueIndexBusy means another session (a replica starting at the same time)
// holds the build lock for this table's sweeper index. It is not a failure:
// the caller must not touch the index and may check again later.
var errDueIndexBusy = errors.New("dispatch: sweeper index is being built by another session")

// dueIndexState reports whether the sweeper index exists and is valid.
func dueIndexState(db *gorm.DB, tableName string) (exists, valid bool, err error) {
	var rows []bool
	if err := db.Raw("SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass(?)",
		pgIndexRef(tableName, dueIndexName(tableName))).Scan(&rows).Error; err != nil {
		return false, false, err
	}
	exists = len(rows) > 0
	return exists, exists && rows[0], nil
}

// dueIndexLockKey is the advisory-lock namespace of one table's sweeper index.
func dueIndexLockKey(tableName string) string { return "metacore.dispatch.due_index:" + tableName }

// ensureDueIndex brings the sweeper index to a valid state.
//
// On Postgres, CREATE INDEX CONCURRENTLY leaves the index INVALID in the catalog
// while it builds. A replica that starts meanwhile must not mistake that for an
// interrupted build and DROP it (it would wait on the builder and then fail with
// "tuple concurrently updated"). So whoever has to create or drop the index
// first takes a session advisory lock on a dedicated connection; if another
// session holds it, the index is theirs to finish and errDueIndexBusy is
// returned (Migrate treats it as success; Wire's background retry re-checks).
// A crashed builder releases the lock with its connection, so a truly stale
// invalid index is still healed by the next starter.
func ensureDueIndex(db *gorm.DB, tableName string) error {
	dialect := db.Dialector.Name()
	if dialect != "postgres" || dbInTx(db) {
		for _, stmt := range dueIndexPlan(dialect, tableName, dbInTx(db), false, false) {
			if err := db.Exec(stmt).Error; err != nil {
				return err
			}
		}
		return nil
	}
	exists, valid, err := dueIndexState(db, tableName)
	if err != nil {
		return err
	}
	if exists && valid {
		return nil
	}
	return db.Connection(func(c *gorm.DB) error {
		key := dueIndexLockKey(tableName)
		var got bool
		if err := c.Raw("SELECT pg_try_advisory_lock(hashtextextended(?, 0))", key).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return errDueIndexBusy
		}
		ctx := c.Statement.Context
		if ctx == nil {
			ctx = context.Background()
		}
		// The lock is per SESSION: whatever happens below, it must be released
		// on this very connection or discarded with it. Cleanup therefore runs
		// on a fresh gorm session (c carries c.Error after a failed statement
		// and gorm would skip the Exec) and on a context that survives the
		// cancellation of Wire's.
		cleanup := func() *gorm.DB {
			u := c.Session(&gorm.Session{NewDB: true}).WithContext(context.WithoutCancel(ctx))
			u.Error = nil // Session copies the sticky error of c
			return u
		}
		discard := func() {
			if conn, ok := c.Statement.ConnPool.(*sql.Conn); ok {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		defer func() {
			var released bool
			if err := cleanup().Raw("SELECT pg_advisory_unlock(hashtextextended(?, 0))", key).Scan(&released).Error; err != nil || !released {
				// Never hand a connection that may still hold the session lock
				// back to the pool: close it so Postgres drops the lock.
				discard()
			}
		}()
		// The build waits for every older snapshot; the host's statement_timeout
		// (ops sets 5 s on every connection) would abort it midway and leave an
		// INVALID index. The dedicated connection runs without that limit (the
		// Wire context still cancels it) and gets the previous value back.
		var prevTimeout string
		if err := c.Raw("SELECT current_setting('statement_timeout')").Scan(&prevTimeout).Error; err != nil {
			return err
		}
		if err := c.Exec("SET statement_timeout = 0").Error; err != nil {
			return err
		}
		defer func() {
			if err := cleanup().Exec("SELECT set_config('statement_timeout', ?, false)", prevTimeout).Error; err != nil {
				discard()
			}
		}()
		// Re-read under the lock: the previous holder may have just finished.
		exists, valid, err := dueIndexState(c, tableName)
		if err != nil {
			return err
		}
		for _, stmt := range dueIndexPlan(dialect, tableName, false, exists, valid) {
			if err := c.Exec(stmt).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
