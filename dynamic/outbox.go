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
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
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
	EventName      string     `gorm:"size:512"`
	Payload        string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index"`
	PublishedAt    *time.Time `gorm:"index"`
	Attempts       int
	LastError      string `gorm:"size:500"`

	// FannedOut records that an in-line (or relay) publication already ran the
	// plain subscribers (audit tap, bridges, ...) and only the durable ledger
	// write failed. The relay then calls PublishDurableOnly instead of
	// re-publishing to everyone. Additive: rows written before the column
	// existed are false, i.e. "unknown", and get the full publication (the
	// safe side for a process that died before fan-out).
	FannedOut bool `gorm:"not null;default:false"`
	// NextAttemptAt is the relay's exponential backoff: the row is not swept
	// before it. NULL (old rows, never-retried rows) means "due".
	NextAttemptAt *time.Time `gorm:"index"`

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
	AggregateType  string    `gorm:"size:256;primaryKey"`
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
	// visible (published_at NULL, attempts capped) instead of disappearing, and
	// the moment it is exhausted it is reported loudly (see reportExhausted).
	outboxMaxAttempts = 25
	// outboxBackoffBase/Max shape the retry spacing: base*2^attempts, capped.
	outboxBackoffBase = 30 * time.Second
	outboxBackoffMax  = 30 * time.Minute
	// MaxEventNameLen is the longest event name the outbox and the dispatch
	// ledger (event_deliveries.event, size:512) can store. A longer one can never
	// be recorded, so it is rejected before persisting instead of burning every
	// relay attempt on an error that cannot heal.
	MaxEventNameLen = 512
	// outboxRetention prunes PUBLISHED rows older than this.
	outboxRetention = 7 * 24 * time.Hour
	// outboxSweepBatch bounds one relay pass.
	outboxSweepBatch = 200
)

// outboxBackoff is the wait before relay attempt number attempts+1.
func outboxBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= 10 {
		return outboxBackoffMax
	}
	d := outboxBackoffBase << attempts
	if d > outboxBackoffMax || d <= 0 {
		return outboxBackoffMax
	}
	return d
}

// ErrEventNameTooLong is returned/logged when an event name exceeds
// MaxEventNameLen. It is permanent: retrying cannot help.
var ErrEventNameTooLong = fmt.Errorf("dynamic: event name longer than %d characters cannot be recorded in the outbox or the dispatch ledger", MaxEventNameLen)

// DurableOnlyPublisher is implemented by a Publisher (events.Bus) that can
// republish an event to its durable routers alone. The relay uses it for a row
// whose plain subscribers already ran.
type DurableOnlyPublisher interface {
	PublishDurableOnly(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) error
}

// siblingsRan reports whether err is a durable-delivery failure raised by a
// publication in which the plain subscribers did run (events.DurableError).
func siblingsRan(err error) bool {
	var d interface{ SiblingsRan() bool }
	return err != nil && errors.As(err, &d) && d.SiblingsRan()
}

// republish sends one persisted row to the bus. A row already marked FannedOut
// goes to the durable routers only (when the bus supports it); an unmarked one
// (process died before/during fan-out) gets the full publication.
func republish(ctx context.Context, bus Publisher, row *EventOutbox) error {
	pctx := row.publishContext(ctx)
	payload := json.RawMessage(row.Payload)
	if row.FannedOut {
		if d, ok := bus.(DurableOnlyPublisher); ok {
			return d.PublishDurableOnly(pctx, row.AddonKey, row.EventName, row.OrganizationID, payload)
		}
	}
	return bus.Publish(pctx, row.AddonKey, row.EventName, row.OrganizationID, payload)
}

// markOutboxFannedOut records that the plain subscribers already ran for id.
func markOutboxFannedOut(ctx context.Context, db *gorm.DB, id uuid.UUID) {
	if err := db.WithContext(ctx).Model(&EventOutbox{}).Where("id = ?", id).
		Update("fanned_out", true).Error; err != nil {
		log.Printf("dynamic: outbox mark fanned_out failed (row %s; the relay may re-run plain subscribers): %v", id, err)
	}
}

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
		Where("published_at IS NULL AND created_at < ? AND attempts < ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)",
			cutoff, outboxMaxAttempts, time.Now().UTC()).
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
		// occurrence id is preserved and the ledger dedups the fan-out. A row
		// whose plain subscribers already ran only goes to the durable routers.
		perr := republish(ctx, s.bus, row)
		attempts := row.Attempts + 1
		updates := map[string]any{"attempts": attempts}
		if perr == nil {
			updates["published_at"] = time.Now().UTC()
			updates["last_error"] = ""
			updates["next_attempt_at"] = nil
			log.Printf("dynamic: outbox relay recovered event %s (org %s, row %s)", row.EventName, row.OrganizationID, row.ID)
		} else {
			msg := perr.Error()
			if len(msg) > 500 {
				msg = msg[:500]
			}
			updates["last_error"] = msg
			updates["next_attempt_at"] = time.Now().UTC().Add(outboxBackoff(attempts))
			if siblingsRan(perr) {
				// The full publication just reached the plain subscribers:
				// from now on only the durable routers are retried.
				updates["fanned_out"] = true
			}
		}
		s.db.WithContext(ctx).Model(&EventOutbox{}).Where("id = ?", row.ID).Updates(updates)
		if perr != nil && attempts >= outboxMaxAttempts {
			row.Attempts, row.LastError = attempts, fmt.Sprint(updates["last_error"])
			s.reportExhausted(ctx, row)
		}
	}
	// Retention: published rows are audit breadcrumbs, not a queue.
	s.db.WithContext(ctx).
		Where("published_at IS NOT NULL AND published_at < ?", time.Now().UTC().Add(-outboxRetention)).
		Delete(&EventOutbox{})
}

// reportExhausted is the loud, structured signal that a row ran out of relay
// attempts and will never be retried again: an error log line
// (`kernel.outbox.exhausted`) and the optional host hook (Config.OnOutboxExhausted),
// which is where a host opens an incident (the incidents package needs a Store,
// an org and a fingerprint policy the dynamic service does not own). The row
// stays in the table (published_at NULL) for manual replay.
func (s *Service) reportExhausted(ctx context.Context, row *EventOutbox) {
	log.Printf("level=error msg=kernel.outbox.exhausted event=%s org=%s row=%s attempts=%d last_error=%q",
		row.EventName, row.OrganizationID, row.ID, row.Attempts, row.LastError)
	if s.onOutboxExhausted != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("dynamic: OnOutboxExhausted panicked: %v", r)
				}
			}()
			s.onOutboxExhausted(ctx, *row)
		}()
	}
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
//
// Inside a caller's transaction (tx != nil) it NEVER fans out in line: the
// event is not real until that transaction commits. The rows (and their
// aggregate sequence bump) join the caller's tx, so a rollback removes them
// and nothing is ever delivered. Publication happens only after commit, via
// the CommitHook the caller put on the ctx (WithCommitHook + CommitHook.Run);
// with no hook the committed rows wait for the outbox relay
// (Service.RelayOutboxOnce), which is the safety net either way.
type OutboxPublisher struct {
	db  *gorm.DB
	bus Publisher

	armed    atomic.Bool
	armMu    sync.Mutex // serialises migration attempts
	armFails int
	retryAt  time.Time
	arming   atomic.Bool  // an async arm attempt is in flight
	crashAt  func() error // test seam: runs between persist and publish
}

// NewOutboxPublisher builds a publisher over db + bus. A nil db degrades to
// direct publication (no crash durability), which is what hosts without a
// database get today. Call Arm at wiring time so the table migration never
// happens on the hot path.
func NewOutboxPublisher(db *gorm.DB, bus Publisher) *OutboxPublisher {
	return &OutboxPublisher{db: db, bus: bus}
}

// OnPersisted installs a fault-injection hook that runs after the events are
// persisted and before any is published; a non-nil error aborts the publication
// exactly as a process crash at that point would (the rows stay unpublished and
// the relay must recover them). For tests only.
func (p *OutboxPublisher) OnPersisted(fn func() error) { p.crashAt = fn }

const (
	armBackoffMin = time.Second
	armBackoffMax = time.Minute
)

// Arm migrates the outbox tables so persistence can be used. It is meant for
// wiring time (WithBus / WithDB / WithEventPublisher), not the hot path. A
// failure is not permanent: the next call after an exponential backoff
// (1s..1m) tries again. Safe for concurrent use.
func (p *OutboxPublisher) Arm() bool {
	if p == nil || p.db == nil {
		return false
	}
	if p.armed.Load() {
		return true
	}
	p.armMu.Lock()
	defer p.armMu.Unlock()
	if p.armed.Load() {
		return true
	}
	if time.Now().Before(p.retryAt) {
		return false
	}
	if err := MigrateEventOutbox(p.db); err != nil {
		p.armFails++
		d := armBackoffMin << min(p.armFails-1, 6)
		if d > armBackoffMax {
			d = armBackoffMax
		}
		p.retryAt = time.Now().Add(d)
		log.Printf("dynamic: event outbox unavailable for guest events (migrate failed: %v) — publishing without crash durability; retrying in %s", err, d)
		return false
	}
	p.armed.Store(true)
	return true
}

// armAsync retries Arm off the caller's goroutine (used when the hot path
// finds the publisher unarmed while a transaction holds a connection: a
// migration there could wait on the very connection the caller owns).
func (p *OutboxPublisher) armAsync() {
	if p.db == nil || !p.arming.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer p.arming.Store(false)
		p.Arm()
	}()
}

// Publish persists then fans out evs in order. tx, when non-nil, is the open
// transaction of the surrounding action: the rows join it, nothing is
// published in line, and the fan-out is deferred to the ctx's CommitHook (see
// OutboxPublisher). It never returns an error to the caller — by the time it
// runs the invocation already succeeded — but it logs every degradation.
func (p *OutboxPublisher) Publish(ctx context.Context, tx *gorm.DB, evs []StagedEvent) {
	if p == nil || p.bus == nil || len(evs) == 0 {
		return
	}
	if evs = rejectOversized(evs); len(evs) == 0 {
		return
	}
	if tx != nil {
		p.publishInTx(ctx, tx, evs)
		return
	}
	var rows []*EventOutbox
	if p.db != nil && p.Arm() {
		rows = p.persist(ctx, p.db, evs)
	}
	if len(rows) > 0 && p.crashAt != nil {
		if err := p.crashAt(); err != nil {
			return
		}
	}
	p.fanOut(ctx, evs, rows)
}

// rejectOversized drops (loudly) the events whose name no store can hold. The
// ledger would fail the INSERT forever; failing here, before persisting, keeps
// the rest of the batch durable and spends no relay attempts.
func rejectOversized(evs []StagedEvent) []StagedEvent {
	ok := evs[:0:0]
	for _, e := range evs {
		if len(e.Name) > MaxEventNameLen {
			log.Printf("level=error msg=kernel.outbox.rejected reason=event_name_too_long len=%d max=%d event=%.80s: %v",
				len(e.Name), MaxEventNameLen, e.Name, ErrEventNameTooLong)
			continue
		}
		ok = append(ok, e)
	}
	return ok
}

// persist writes evs to the outbox as one (nested) transaction on db; nil on
// failure (logged), in which case the caller degrades to non-durable fan-out.
func (p *OutboxPublisher) persist(ctx context.Context, db *gorm.DB, evs []StagedEvent) []*EventOutbox {
	rows := make([]*EventOutbox, 0, len(evs))
	perr := db.WithContext(ctx).Transaction(func(t *gorm.DB) error {
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
		return nil
	}
	return rows
}

func (p *OutboxPublisher) publishInTx(ctx context.Context, tx *gorm.DB, evs []StagedEvent) {
	var rows []*EventOutbox
	if p.armed.Load() {
		rows = p.persist(ctx, tx, evs)
	} else {
		// Never migrate while the caller's tx holds a connection.
		p.armAsync()
	}
	hook := CommitHookFrom(ctx)
	if hook == nil {
		if len(rows) > 0 {
			log.Printf("dynamic: %d guest event(s) persisted in the caller's transaction; no commit hook on the ctx, the outbox relay publishes them after commit", len(rows))
		} else {
			log.Printf("dynamic: %d guest event(s) NOT persisted and no commit hook on the ctx: dropped (publishing in line would announce uncommitted state)", len(evs))
		}
		return
	}
	staged := append([]StagedEvent(nil), evs...)
	hook.add(func(c context.Context) {
		if len(rows) > 0 && p.crashAt != nil {
			if err := p.crashAt(); err != nil {
				return
			}
		}
		p.fanOut(c, staged, rows)
	})
}

// fanOut publishes evs (rows[i] is the persisted row of evs[i], when any) and
// stamps published_at on the persisted ones.
func (p *OutboxPublisher) fanOut(ctx context.Context, evs []StagedEvent, rows []*EventOutbox) {
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
			if row != nil && p.db != nil && siblingsRan(err) {
				// Plain subscribers already ran: the relay must retry the
				// durable routers only.
				markOutboxFannedOut(ctx, p.db, row.ID)
			}
			continue
		}
		if row != nil && p.db != nil {
			p.db.WithContext(ctx).Model(&EventOutbox{}).Where("id = ?", row.ID).
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
	if len(e.Name) > MaxEventNameLen {
		return nil, ErrEventNameTooLong
	}
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
		// A guest-declared aggregate counts in its emitter's own namespace
		// ("<addon>:<type>"; addon keys never contain ':'), so one addon can
		// never advance another's counter, nor a canonical model's.
		seqType := e.AggregateType
		if e.AddonKey != "" {
			seqType = e.AddonKey + ":" + e.AggregateType
		}
		seq, err := nextAggregateSequence(ctx, t, e.OrgID, seqType, e.AggregateID)
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
