package dynamic

// Transactional-outbox durability for canonical events.
//
// publishCanonical used to hand the event straight to the in-process bus: a
// crash (or bus panic) between the committed DB write and the fan-out lost
// the event forever — the data existed but no subscriber ever heard about it,
// and only host-level reconcilers ("healers") could notice. The outbox closes
// that window the way large event-driven systems do:
//
//  1. The event is PERSISTED to kernel_event_outbox before any fan-out.
//  2. The bus publish happens immediately after; on success the row is
//     stamped published_at.
//  3. A background relay sweeps rows that never got stamped (crash mid-way,
//     bus failure) and re-publishes them. Downstream the dispatch ledger's
//     ON CONFLICT(delivery_id) dedup makes the replay collapse into the
//     already-recorded deliveries, so at-least-once stays exactly-once-ish
//     per subscriber.
//
// The outbox arms itself automatically when a Bus is wired and the table
// migrates; a host that cannot migrate (read-only DB) degrades to the old
// direct-publish behaviour with one loud log line.

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// EventOutbox is one durably-persisted canonical event awaiting (or done
// with) bus fan-out. Rows are small and pruned after retention (see relay).
type EventOutbox struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID  `gorm:"type:uuid;index"`
	AddonKey       string     `gorm:"size:100"`
	EventName      string     `gorm:"size:200"`
	Payload        string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index"`
	PublishedAt    *time.Time `gorm:"index"`
	Attempts       int
	LastError      string `gorm:"size:500"`

	// Envelope identity (metacore.event/1). All additive and nullable: rows
	// written before these columns existed relay exactly as before.
	//
	// EventID is the publication id; it is the idempotency discriminator the
	// dispatch ledger keys on, so a relay re-publish of this row collapses into
	// the deliveries already recorded. Sequence is the per-aggregate monotonic
	// counter allocated when the row is persisted.
	EventID       string `gorm:"size:64"`
	Version       int
	OccurredAt    *time.Time
	AggregateType string `gorm:"size:200"`
	AggregateID   string `gorm:"size:200"`
	Sequence      int64
	CausationID   string `gorm:"size:64"`
	CorrelationID string `gorm:"size:128"`
	ActorID       string `gorm:"size:64"`
	BranchID      string `gorm:"size:64"`
}

// EventSequence is the per-aggregate counter behind the envelope's `sequence`.
// One row per (org, aggregate type, aggregate id), bumped atomically by an
// upsert, so two concurrent events about the same aggregate never share a
// number.
type EventSequence struct {
	OrganizationID uuid.UUID `gorm:"type:uuid;primaryKey"`
	AggregateType  string    `gorm:"size:200;primaryKey"`
	AggregateID    string    `gorm:"size:200;primaryKey"`
	LastSeq        int64
	UpdatedAt      time.Time
}

// TableName pins the physical name; kernel-owned, host-schema table.
func (EventSequence) TableName() string { return "kernel_event_sequences" }

// TableName pins the physical name; kernel-owned, host-schema table.
func (EventOutbox) TableName() string { return "kernel_event_outbox" }

// MigrateEventOutbox creates/updates the outbox table. Additive and safe to
// call repeatedly.
func MigrateEventOutbox(db *gorm.DB) error {
	return db.AutoMigrate(&EventOutbox{}, &EventSequence{})
}

const (
	// outboxRelayInterval is how often the relay sweeps for unpublished rows.
	outboxRelayInterval = 30 * time.Second
	// outboxRelayMinAge keeps the relay from racing the in-line publish that
	// is stamping published_at right now.
	outboxRelayMinAge = 10 * time.Second
	// outboxMaxAttempts caps relay retries per row; an exhausted row stays
	// visible (published_at NULL, attempts capped) instead of disappearing.
	outboxMaxAttempts = 25
	// outboxRetention prunes PUBLISHED rows older than this.
	outboxRetention = 7 * 24 * time.Hour
	// outboxSweepBatch bounds one relay pass.
	outboxSweepBatch = 200
)

// persistOutbox writes the pre-publish intent row. Failure is loud but
// non-fatal: the event still fans out directly (old behaviour), it just
// isn't crash-durable for this one publication.
func (s *Service) persistOutbox(ctx context.Context, row *EventOutbox) bool {
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		log.Printf("dynamic: outbox persist failed (event %s fans out without crash durability): %v", row.EventName, err)
		return false
	}
	return true
}

// markOutboxPublished stamps a successfully fanned-out row.
func (s *Service) markOutboxPublished(ctx context.Context, id uuid.UUID) {
	now := time.Now().UTC()
	s.db.WithContext(ctx).Model(&EventOutbox{}).
		Where("id = ?", id).
		Updates(map[string]any{"published_at": now})
}

// StartOutboxRelay launches the background sweeper. Called from New when the
// outbox armed; idempotent via the service's stop channel (one relay per
// Service). StopOutboxRelay ends it.
func (s *Service) StartOutboxRelay() {
	if s.outboxStop != nil {
		return
	}
	s.outboxStop = make(chan struct{})
	go s.outboxRelayLoop(s.outboxStop)
}

// StopOutboxRelay terminates the background sweeper (graceful shutdown /
// tests).
func (s *Service) StopOutboxRelay() {
	if s.outboxStop != nil {
		close(s.outboxStop)
		s.outboxStop = nil
	}
}

func (s *Service) outboxRelayLoop(stop <-chan struct{}) {
	t := time.NewTicker(outboxRelayInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.RelayOutboxOnce(context.Background())
		}
	}
}

// RelayOutboxOnce performs one sweep: re-publish unpublished rows old enough
// to not be mid-flight, then prune old published rows. Exported so hosts /
// tests can force a pass.
func (s *Service) RelayOutboxOnce(ctx context.Context) {
	if s.bus == nil {
		return
	}
	var rows []EventOutbox
	cutoff := time.Now().UTC().Add(-outboxRelayMinAge)
	err := s.db.WithContext(ctx).
		Where("published_at IS NULL AND created_at < ? AND attempts < ?", cutoff, outboxMaxAttempts).
		Order("created_at asc").
		Limit(outboxSweepBatch).
		Find(&rows).Error
	if err != nil {
		log.Printf("dynamic: outbox relay query failed: %v", err)
		return
	}
	for i := range rows {
		row := &rows[i]
		// Re-publish the ORIGINAL bytes: the dispatcher re-marshals payloads,
		// and json.RawMessage round-trips byte-identical, so the canonical
		// occurrence id is preserved and the ledger dedups the fan-out.
		perr := s.bus.Publish(row.publishContext(ctx), row.AddonKey, row.EventName, row.OrganizationID, json.RawMessage(row.Payload))
		updates := map[string]any{"attempts": row.Attempts + 1}
		if perr == nil {
			updates["published_at"] = time.Now().UTC()
			updates["last_error"] = ""
			log.Printf("dynamic: outbox relay recovered event %s (org %s, row %s)", row.EventName, row.OrganizationID, row.ID)
		} else {
			msg := perr.Error()
			if len(msg) > 500 {
				msg = msg[:500]
			}
			updates["last_error"] = msg
		}
		s.db.WithContext(ctx).Model(&EventOutbox{}).Where("id = ?", row.ID).Updates(updates)
	}
	// Retention: published rows are audit breadcrumbs, not a queue.
	s.db.WithContext(ctx).
		Where("published_at IS NOT NULL AND published_at < ?", time.Now().UTC().Add(-outboxRetention)).
		Delete(&EventOutbox{})
}

// publishContext rebuilds the publication context the original publisher had:
// the stable envelope identity (Meta) and the actor / branch /
// correlation / causation the dispatcher re-attaches to the delivery ctx. A
// relay re-publish therefore carries the same identity as the first attempt,
// which is what makes the ledger collapse it into the deliveries it already has.
func (r *EventOutbox) publishContext(ctx context.Context) context.Context {
	m := Meta{
		ID:            r.EventID,
		Version:       r.Version,
		AggregateType: r.AggregateType,
		AggregateID:   r.AggregateID,
		Sequence:      r.Sequence,
		CausationID:   r.CausationID,
	}
	if r.OccurredAt != nil {
		m.OccurredAt = r.OccurredAt.UTC().Format(time.RFC3339Nano)
	}
	ctx = WithMeta(ctx, m)
	ctx = WithCausationID(ctx, r.CausationID)
	ctx = WithActorID(ctx, r.ActorID)
	ctx = WithBranchID(ctx, r.BranchID)
	if r.CorrelationID != "" {
		ctx = WithCorrelationID(ctx, r.CorrelationID)
	}
	return ctx
}

// nextAggregateSequence atomically bumps and returns the counter of one
// aggregate. It is a single upsert, so it is safe under any concurrency and
// holds the row lock only until the surrounding transaction ends.
func nextAggregateSequence(ctx context.Context, db *gorm.DB, orgID uuid.UUID, aggType, aggID string) (int64, error) {
	var seq int64
	err := db.WithContext(ctx).Raw(
		`INSERT INTO kernel_event_sequences (organization_id, aggregate_type, aggregate_id, last_seq, updated_at)
		 VALUES (?, ?, ?, 1, ?)
		 ON CONFLICT (organization_id, aggregate_type, aggregate_id)
		 DO UPDATE SET last_seq = kernel_event_sequences.last_seq + 1, updated_at = excluded.updated_at
		 RETURNING last_seq`,
		orgID, aggType, aggID, time.Now().UTC()).Scan(&seq).Error
	return seq, err
}

// StagedEvent is one domain event an emitter (the wasm event_emit import)
// buffered during an invocation and now wants published durably.
type StagedEvent struct {
	ID            string // publication id; generated when empty
	AddonKey      string // emitter (bus capability subject)
	Name          string
	OrgID         uuid.UUID
	Payload       []byte // raw JSON, as the guest wrote it
	Version       int
	AggregateType string
	AggregateID   string
	OccurredAt    time.Time
	CausationID   string
	CorrelationID string
	ActorID       string
	BranchID      string
}

// OutboxPublisher publishes buffered domain events through the same outbox the
// canonical CRUD events use: persist first, fan out second, stamp
// published_at on success. A crash between the persist and the fan-out leaves
// the row unpublished, and Service.RelayOutboxOnce re-publishes it.
type OutboxPublisher struct {
	db  *gorm.DB
	bus Publisher

	once    sync.Once
	armed   bool
	crashAt func() error // test seam: runs between persist and publish
}

// NewOutboxPublisher builds a publisher over db + bus. A nil db degrades to
// direct publication (no crash durability), which is what hosts without a
// database get today.
func NewOutboxPublisher(db *gorm.DB, bus Publisher) *OutboxPublisher {
	return &OutboxPublisher{db: db, bus: bus}
}

// OnPersisted installs a fault-injection hook that runs after the events are
// persisted and before any is published; a non-nil error aborts the publication
// exactly as a process crash at that point would (the rows stay unpublished and
// the relay must recover them). For tests only.
func (p *OutboxPublisher) OnPersisted(fn func() error) { p.crashAt = fn }

func (p *OutboxPublisher) arm() bool {
	p.once.Do(func() {
		if p.db == nil {
			return
		}
		if err := MigrateEventOutbox(p.db); err != nil {
			log.Printf("dynamic: event outbox unavailable for guest events (migrate failed: %v) — publishing without crash durability", err)
			return
		}
		p.armed = true
	})
	return p.armed
}

// Publish persists then fans out evs in order. tx, when non-nil, is the open
// transaction of the surrounding action: the rows join it, so a rollback of
// the action also removes the events it staged. It never returns an error to
// the caller — by the time it runs the invocation already succeeded — but it
// logs every degradation.
func (p *OutboxPublisher) Publish(ctx context.Context, tx *gorm.DB, evs []StagedEvent) {
	if p == nil || p.bus == nil || len(evs) == 0 {
		return
	}
	rows := make([]*EventOutbox, 0, len(evs))
	dbx := p.db
	if tx != nil {
		dbx = tx
	}
	if dbx != nil && p.arm() {
		perr := dbx.WithContext(ctx).Transaction(func(t *gorm.DB) error {
			rows = rows[:0]
			for i := range evs {
				row, err := p.stage(ctx, t, &evs[i])
				if err != nil {
					return err
				}
				rows = append(rows, row)
			}
			return nil
		})
		if perr != nil {
			log.Printf("dynamic: guest event outbox persist failed (%d event(s) fan out without crash durability): %v", len(evs), perr)
			rows = nil
		}
	}
	if len(rows) > 0 && p.crashAt != nil {
		if err := p.crashAt(); err != nil {
			return
		}
	}
	for i := range evs {
		var row *EventOutbox
		if i < len(rows) {
			row = rows[i]
		}
		var pctx context.Context
		var payload json.RawMessage
		if row != nil {
			pctx = row.publishContext(ctx)
			payload = json.RawMessage(row.Payload)
		} else {
			e := evs[i]
			tmp := EventOutbox{EventID: e.ID, Version: e.Version, AggregateType: e.AggregateType,
				AggregateID: e.AggregateID, CausationID: e.CausationID, CorrelationID: e.CorrelationID,
				ActorID: e.ActorID, BranchID: e.BranchID}
			if !e.OccurredAt.IsZero() {
				t := e.OccurredAt
				tmp.OccurredAt = &t
			}
			pctx = tmp.publishContext(ctx)
			payload = json.RawMessage(normalizePayload(e.Payload))
		}
		if err := p.bus.Publish(pctx, evs[i].AddonKey, evs[i].Name, evs[i].OrgID, payload); err != nil {
			log.Printf("dynamic: guest event %s publish failed: %v", evs[i].Name, err)
			continue
		}
		if row != nil {
			dbx.WithContext(ctx).Model(&EventOutbox{}).Where("id = ?", row.ID).
				Updates(map[string]any{"published_at": time.Now().UTC()})
		}
	}
}

func normalizePayload(b []byte) string {
	if len(b) == 0 {
		return "null"
	}
	return string(b)
}

func (p *OutboxPublisher) stage(ctx context.Context, t *gorm.DB, e *StagedEvent) (*EventOutbox, error) {
	if e.ID == "" {
		e.ID = NewEventID()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.Version <= 0 {
		e.Version = 1
	}
	occurred := e.OccurredAt.UTC()
	row := &EventOutbox{
		ID:             uuid.New(),
		OrganizationID: e.OrgID,
		AddonKey:       e.AddonKey,
		EventName:      e.Name,
		Payload:        normalizePayload(e.Payload),
		CreatedAt:      time.Now().UTC(),
		EventID:        e.ID,
		Version:        e.Version,
		OccurredAt:     &occurred,
		AggregateType:  e.AggregateType,
		AggregateID:    e.AggregateID,
		CausationID:    e.CausationID,
		CorrelationID:  e.CorrelationID,
		ActorID:        e.ActorID,
		BranchID:       e.BranchID,
	}
	if e.AggregateType != "" && e.AggregateID != "" {
		seq, err := nextAggregateSequence(ctx, t, e.OrgID, e.AggregateType, e.AggregateID)
		if err != nil {
			return nil, err
		}
		row.Sequence = seq
	}
	if err := t.WithContext(ctx).Create(row).Error; err != nil {
		return nil, err
	}
	return row, nil
}

// NewEventID returns a time-ordered (uuidv7) publication id, falling back to
// v4 when the clock/entropy source fails.
func NewEventID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
