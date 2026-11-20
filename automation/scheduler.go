package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/outbox"
)

// runJob is the payload of an automation.run outbox job.
type runJob struct {
	AutomationID uuid.UUID      `json:"automation_id"`
	Slot         string         `json:"slot"`    // RFC 3339 of the scheduled time, or "event:<id>"
	Trigger      string         `json:"trigger"` // cron | interval | once | event
	Event        map[string]any `json:"event,omitempty"`
}

const tickBatch = 50

// Tick claims due automations, enqueues one run per due slot and advances
// next_run_at, all in one transaction. On PostgreSQL candidate rows are taken
// with FOR UPDATE SKIP LOCKED, so concurrent replicas split the work; the
// outbox dedupe key automation:<id>:<slot> makes a second enqueue of the same
// slot a no-op even across restarts. Missed slots (process was down) collapse
// into a single run and the schedule jumps to the next future occurrence.
func (s *Service) Tick(ctx context.Context) (int, error) {
	if !s.policy.Enabled || s.ob == nil {
		return 0, nil
	}
	now := s.now().UTC()
	enqueued := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Where("status = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", StatusActive, now).
			Order("next_run_at").Limit(tickBatch)
		if tx.Dialector.Name() == "postgres" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		var due []Automation
		if err := q.Find(&due).Error; err != nil {
			return err
		}
		for i := range due {
			a := &due[i]
			spec, err := a.ParsedSpec()
			if err != nil {
				s.markError(tx, a, "unreadable spec")
				continue
			}
			slot := a.NextRunAt.UTC().Format(time.RFC3339)
			job := runJob{AutomationID: a.ID, Slot: slot, Trigger: a.TriggerType}
			if _, err := s.ob.EnqueueTx(tx, JobRun, job,
				outbox.WithOrg(a.OrganizationID),
				outbox.WithDedupeKey(fmt.Sprintf("automation:%s:%s", a.ID, slot)),
				outbox.WithMaxAttempts(s.maxAtt)); err != nil && !errors.Is(err, outbox.ErrDuplicate) {
				return err
			}
			enqueued++
			updates := map[string]any{"next_run_at": nil, "updated_at": now}
			if next, ok, err := NextRun(spec.Trigger, maxTime(now, *a.NextRunAt), s.policy.DefaultTimezone); err == nil && ok {
				updates["next_run_at"] = next.UTC()
			}
			if err := tx.Model(&Automation{}).Where("id = ?", a.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return enqueued, err
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// StartScheduler ticks every `every` (default 30s) until ctx is cancelled and
// returns a stop function that waits for the loop to exit.
func (s *Service) StartScheduler(ctx context.Context, every time.Duration) (stop func()) {
	if every <= 0 {
		every = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if n, err := s.safeTick(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("automation scheduler tick", "err", err)
			} else if n > 0 {
				s.log.Info("automation scheduler", "enqueued", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (s *Service) safeTick(ctx context.Context) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return s.Tick(ctx)
}

// ---- events ----------------------------------------------------------------

// EnqueueEventTx records a domain event inside the caller's transaction (the
// transactional-outbox pattern): if the domain change rolls back, no
// automation fires. The event is matched asynchronously.
func (s *Service) EnqueueEventTx(tx *gorm.DB, ev Event) error {
	if s.ob == nil || !s.policy.Enabled {
		return nil
	}
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	_, err := s.ob.EnqueueTx(tx, JobEvent, ev, outbox.WithOrg(ev.OrganizationID),
		outbox.WithDedupeKey("automation-event:"+ev.Name+":"+ev.ID))
	if errors.Is(err, outbox.ErrDuplicate) {
		return nil
	}
	return err
}

// Ingest matches an event against active event automations of its org and
// enqueues one run per match. Idempotent per (automation, event id); a
// cooldown (trigger.cooldown or Policy.MinInterval) bounds bursts.
func (s *Service) Ingest(ctx context.Context, ev Event) (int, error) {
	if !s.policy.Enabled || s.ob == nil || ev.OrganizationID == uuid.Nil || ev.Name == "" {
		return 0, nil
	}
	if ev.ID == "" {
		return 0, errors.New("automation: event id required")
	}
	var cands []Automation
	if err := s.db.WithContext(ctx).Where("organization_id = ? AND status = ? AND kind = ? AND trigger_type = ?",
		ev.OrganizationID, StatusActive, KindAutomation, TriggerEvent).Find(&cands).Error; err != nil {
		return 0, err
	}
	now := s.now().UTC()
	n := 0
	for i := range cands {
		a := &cands[i]
		spec, err := a.ParsedSpec()
		if err != nil || spec.Trigger == nil || !eventMatches(spec.Trigger.Event, ev.Name) {
			continue
		}
		if !conditionsMatch(spec.Trigger.Where, ev.Payload) {
			continue
		}
		cooldown := s.policy.MinInterval
		if spec.Trigger.Cooldown != "" {
			if d, err := time.ParseDuration(spec.Trigger.Cooldown); err == nil {
				cooldown = d
			}
		}
		if a.LastRunAt != nil && now.Sub(*a.LastRunAt) < cooldown {
			continue
		}
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			res := tx.Clauses(onConflictNothing).Create(&EventRecord{AutomationID: a.ID, EventKey: ev.Name + ":" + ev.ID, CreatedAt: now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return nil // already fired for this event
			}
			// Claim the cooldown window now so a burst of events in the same
			// second does not enqueue several runs.
			upd := tx.Model(&Automation{}).Where("id = ? AND (last_run_at IS NULL OR last_run_at <= ?)", a.ID, now.Add(-cooldown)).
				Update("last_run_at", now)
			if upd.Error != nil {
				return upd.Error
			}
			if upd.RowsAffected == 0 {
				return nil
			}
			payload := map[string]any{"id": ev.ID, "name": ev.Name, "payload": ev.Payload}
			if !ev.OccurredAt.IsZero() {
				payload["occurred_at"] = ev.OccurredAt.UTC().Format(time.RFC3339)
			}
			_, err := s.ob.EnqueueTx(tx, JobRun, runJob{AutomationID: a.ID, Slot: "event:" + ev.ID, Trigger: TriggerEvent, Event: payload},
				outbox.WithOrg(a.OrganizationID),
				outbox.WithDedupeKey(fmt.Sprintf("automation:%s:event:%s", a.ID, ev.ID)),
				outbox.WithMaxAttempts(s.maxAtt))
			if err != nil && !errors.Is(err, outbox.ErrDuplicate) {
				return err
			}
			n++
			return nil
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *Service) handleEventJob(ctx context.Context, job outbox.Job) error {
	var ev Event
	if err := job.Decode(&ev); err != nil {
		return outbox.Permanent(err)
	}
	_, err := s.Ingest(ctx, ev)
	return err
}

// eventMatches supports exact names and a trailing ".*" wildcard.
func eventMatches(pattern, name string) bool {
	if pattern == name || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func conditionsMatch(conds []Condition, payload map[string]any) bool {
	for _, c := range conds {
		v, _ := lookup(payload, c.Field)
		if !compare(v, c.Op, c.Value) {
			return false
		}
	}
	return true
}

// markError moves an automation to the error state (auto-pause).
func (s *Service) markError(tx *gorm.DB, a *Automation, reason string) {
	tx.Model(&Automation{}).Where("id = ?", a.ID).Updates(map[string]any{
		"status": StatusError, "status_reason": truncate(reason, 300), "next_run_at": nil, "updated_at": s.now().UTC(),
	})
	s.audit(context.Background(), a, uuid.Nil, "error", reason)
}

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// jsonOf marshals v to a string ("" on error).
func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
