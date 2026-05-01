package eventlog

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/log"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Event is the persisted row shape. It embeds modelbase.BaseUUIDModel so
// every row gets a UUID, the tenant scope, timestamps and soft-delete column
// (the service never issues soft-deletes — events are immutable — but the
// column exists for consistency with the rest of the platform).
//
// Tags is the generic correlation bag: string→string pairs stored as JSONB.
// Apps build typed helpers that translate their domain IDs into tag entries
// (see package docs for the recommended wrapper pattern). Data is the
// free-form payload for the event body.
//
// The (organization_id, sequence_num) pair is the durable ordering key:
// apps paginate and resume SSE streams using SequenceNum as their
// Last-Event-ID.
type Event struct {
	modelbase.BaseUUIDModel
	SequenceNum int64                  `json:"sequence_num" gorm:"index:idx_eventlog_org_seq,priority:2;autoIncrement:false"`
	EventType   string                 `json:"event_type" gorm:"size:100;index;not null"`
	Tags        map[string]string      `json:"tags,omitempty" gorm:"serializer:json;type:jsonb"`
	Data        map[string]interface{} `json:"data" gorm:"serializer:json;type:jsonb"`
}

// TableName pins the table so consumers get the same schema everywhere.
func (Event) TableName() string { return "eventlog_events" }

// EventOption is a functional option for configuring an Event at Emit time.
type EventOption func(*Event)

// WithTag sets a single correlation tag. Overwrites any prior value for key.
func WithTag(key, value string) EventOption {
	return func(e *Event) {
		if e.Tags == nil {
			e.Tags = make(map[string]string, 1)
		}
		e.Tags[key] = value
	}
}

// WithTags merges every entry in tags into the event's tag map. Later keys
// overwrite earlier ones.
func WithTags(tags map[string]string) EventOption {
	return func(e *Event) {
		if len(tags) == 0 {
			return
		}
		if e.Tags == nil {
			e.Tags = make(map[string]string, len(tags))
		}
		for k, v := range tags {
			e.Tags[k] = v
		}
	}
}

// QueryParams drives cursor-based reads over the event log for a given org.
//
// AfterSequence is the strict lower bound (events with sequence_num > N).
// Types filters by event_type IN (…). TagFilters narrows by exact-match
// tag entries — each pair becomes a JSONB containment predicate. Limit
// is clamped to [1, 200] with a default of 50.
type QueryParams struct {
	AfterSequence int64
	Types         []string
	TagFilters    map[string]string
	Limit         int
}

// Cursor is the pagination token returned from Query.
type Cursor struct {
	LastSequence int64 `json:"last_sequence"`
	HasMore      bool  `json:"has_more"`
}

// Service persists events, assigns per-org sequence numbers under a row-level
// lock, and fans out to in-process subscribers for SSE-style streaming.
//
// The subscribers map is (orgID → list-of-channels). Channels are buffered
// (size 100); slow subscribers drop the event rather than blocking Emit —
// they are expected to resume via the Query cursor on reconnection.
type Service struct {
	db          *gorm.DB
	subscribers map[uuid.UUID][]chan *Event
	mu          sync.RWMutex
}

// NewService wires the Service to a *gorm.DB. The DB must already have the
// Event table migrated (see Event.TableName). The kernel does not auto-
// migrate app tables — host owns migration timing.
func NewService(db *gorm.DB) *Service {
	return &Service{
		db:          db,
		subscribers: make(map[uuid.UUID][]chan *Event),
	}
}

// Emit persists an event and broadcasts it to live subscribers. The caller
// supplies the tenant (orgID), the event type ("message.incoming", …), a
// free-form Data map and zero or more EventOption for tag correlation.
//
// Sequencing: the per-org sequence number is assigned inside a transaction
// that takes a row-level UPDATE lock against the org's latest row — so
// concurrent emits for the same org serialize. Different orgs stay
// independent.
//
// The broadcast step is best-effort: slow subscribers whose buffer is full
// will drop this event (logged via kernel/obs). Apps that care about
// guaranteed delivery should reconcile via Query on reconnect.
func (s *Service) Emit(ctx context.Context, orgID uuid.UUID, eventType string, data map[string]interface{}, opts ...EventOption) error {
	if eventType == "" {
		return fmt.Errorf("eventlog: empty event type")
	}

	event := &Event{
		EventType: eventType,
		Data:      data,
	}
	event.OrganizationID = orgID
	for _, opt := range opts {
		opt(event)
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Per-org serialization. Postgres rejects FOR UPDATE on a SELECT with
		// aggregate functions (SQLSTATE 0A000), so we use a transaction-scoped
		// advisory lock keyed on a hash of the org id instead. Released auto-
		// matically on commit/rollback. Other dialects (sqlite test fixtures)
		// rely on the transaction's own isolation since they're single-writer.
		if err := acquireOrgLock(tx, orgID); err != nil {
			return err
		}
		var maxSeq struct {
			Seq int64
		}
		if err := tx.Model(&Event{}).
			Where("organization_id = ?", orgID).
			Select("COALESCE(MAX(sequence_num), 0) as seq").
			Scan(&maxSeq).Error; err != nil {
			return err
		}
		event.SequenceNum = maxSeq.Seq + 1
		return tx.Create(event).Error
	})
	if err != nil {
		log.FromContext(ctx).Error("eventlog.emit.error",
			"org_id", orgID.String(),
			"event_type", eventType,
			"err", err.Error(),
		)
		return err
	}

	s.broadcast(ctx, orgID, event)
	return nil
}

// acquireOrgLock takes a Postgres transaction-scoped advisory lock keyed on
// a hash of orgID, serializing Emit calls for the same tenant while leaving
// other tenants unblocked. Postgres released it on tx commit or rollback,
// no manual unlock needed.
//
// On non-Postgres dialects (eg sqlite used in unit tests) this is a no-op:
// those backends are single-writer or otherwise serialize naturally, and
// pg_advisory_xact_lock isn't portable.
func acquireOrgLock(tx *gorm.DB, orgID uuid.UUID) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", orgID.String()).Error
}

// broadcast fan-outs event to every live subscriber for orgID under the
// read-lock. Slow subscribers are dropped (buffer full) — see Emit doc.
func (s *Service) broadcast(ctx context.Context, orgID uuid.UUID, event *Event) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	channels, ok := s.subscribers[orgID]
	if !ok {
		return
	}
	for _, ch := range channels {
		select {
		case ch <- event:
		default:
			log.FromContext(ctx).Warn("eventlog.subscriber.slow",
				"org_id", orgID.String(),
				"sequence_num", event.SequenceNum,
			)
		}
	}
}

// Subscribe registers a buffered channel for orgID and returns it together
// with an unsubscribe function. Invoke unsubscribe exactly once (idempotent
// protections are not provided) to drain and close the channel. Typical
// usage from an SSE handler:
//
//	ch, unsubscribe := svc.Subscribe(orgID)
//	defer unsubscribe()
//	for evt := range ch { ... }
func (s *Service) Subscribe(orgID uuid.UUID) (<-chan *Event, func()) {
	ch := make(chan *Event, 100)

	s.mu.Lock()
	s.subscribers[orgID] = append(s.subscribers[orgID], ch)
	s.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()

			channels := s.subscribers[orgID]
			for i, c := range channels {
				if c == ch {
					s.subscribers[orgID] = append(channels[:i], channels[i+1:]...)
					close(ch)
					break
				}
			}
			if len(s.subscribers[orgID]) == 0 {
				delete(s.subscribers, orgID)
			}
		})
	}

	return ch, unsubscribe
}

// Query returns at most params.Limit events for orgID, strictly after
// params.AfterSequence, optionally filtered by Types and TagFilters. It
// also returns a Cursor whose LastSequence is the caller's next
// AfterSequence, and HasMore indicating whether more rows exist beyond
// the returned page.
//
// Results are ordered by sequence_num ASC so the caller sees the log in
// monotonic order — SSE replay depends on this.
func (s *Service) Query(ctx context.Context, orgID uuid.UUID, params QueryParams) ([]Event, *Cursor, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	q := s.db.WithContext(ctx).Where("organization_id = ?", orgID)

	if params.AfterSequence > 0 {
		q = q.Where("sequence_num > ?", params.AfterSequence)
	}
	if len(params.Types) > 0 {
		q = q.Where("event_type IN ?", params.Types)
	}
	for k, v := range params.TagFilters {
		// JSONB containment: works on Postgres (production) and is tolerated
		// as a string LIKE by gorm's SQLite driver for tests with simple
		// shapes. Apps that need exotic filtering should query directly.
		q = q.Where("tags @> ?", fmt.Sprintf(`{"%s":"%s"}`, k, v))
	}

	var events []Event
	if err := q.Order("sequence_num ASC").Limit(limit + 1).Find(&events).Error; err != nil {
		return nil, nil, err
	}

	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}

	var lastSeq int64
	switch {
	case len(events) > 0:
		lastSeq = events[len(events)-1].SequenceNum
	case params.AfterSequence > 0:
		lastSeq = params.AfterSequence
	}

	return events, &Cursor{LastSequence: lastSeq, HasMore: hasMore}, nil
}
