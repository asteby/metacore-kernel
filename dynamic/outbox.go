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
}

// TableName pins the physical name; kernel-owned, host-schema table.
func (EventOutbox) TableName() string { return "kernel_event_outbox" }

// MigrateEventOutbox creates/updates the outbox table. Additive and safe to
// call repeatedly.
func MigrateEventOutbox(db *gorm.DB) error {
	return db.AutoMigrate(&EventOutbox{})
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
		perr := s.bus.Publish(ctx, row.AddonKey, row.EventName, row.OrganizationID, json.RawMessage(row.Payload))
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
