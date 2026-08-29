package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// outboxRecorder is a Publisher double that can be told to fail.
type outboxRecorder struct {
	mu     sync.Mutex
	fail   bool
	events []string
	raws   []any
}

func (r *outboxRecorder) Publish(_ context.Context, _ string, event string, _ uuid.UUID, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("bus down")
	}
	r.events = append(r.events, event)
	r.raws = append(r.raws, payload)
	return nil
}

func (r *outboxRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// newOutboxTestService builds a Service against an in-memory DB with the
// outbox armed and the ticker relay STOPPED (tests drive RelayOutboxOnce).
func newOutboxTestService(t *testing.T, bus Publisher) *Service {
	t.Helper()
	db := setupTestDB(t)
	svc := setupService(t, db)
	if err := MigrateEventOutbox(svc.db); err != nil {
		t.Fatalf("migrate outbox: %v", err)
	}
	svc.bus = bus
	svc.outboxEnabled = true
	svc.StopOutboxRelay()
	return svc
}

func TestOutbox_PersistsBeforeFanoutAndMarksPublished(t *testing.T) {
	rec := &outboxRecorder{}
	svc := newOutboxTestService(t, rec)

	user := newUser(uuid.New())
	svc.publishCanonical(context.Background(), "dyn_items", "created", user, uuid.NewString(), nil, map[string]any{"x": 1})

	if rec.count() != 1 {
		t.Fatalf("expected 1 publish, got %d", rec.count())
	}
	var rows []EventOutbox
	if err := svc.db.Find(&rows).Error; err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 outbox row, got %d", len(rows))
	}
	if rows[0].PublishedAt == nil {
		t.Fatalf("expected published_at stamped after successful fan-out")
	}
	var ce CanonicalEvent
	if err := json.Unmarshal([]byte(rows[0].Payload), &ce); err != nil || ce.Action != "created" {
		t.Fatalf("payload should round-trip the canonical event, got err=%v action=%q", err, ce.Action)
	}
}

func TestOutbox_RelayRecoversFailedPublish(t *testing.T) {
	rec := &outboxRecorder{fail: true}
	svc := newOutboxTestService(t, rec)

	user := newUser(uuid.New())
	svc.publishCanonical(context.Background(), "dyn_items", "created", user, uuid.NewString(), nil, map[string]any{"x": 1})

	var row EventOutbox
	if err := svc.db.First(&row).Error; err != nil {
		t.Fatalf("outbox row missing after failed publish: %v", err)
	}
	if row.PublishedAt != nil {
		t.Fatalf("row must stay unpublished when the bus failed")
	}

	// Age the row past the relay's min-age guard, heal the bus, sweep.
	svc.db.Model(&EventOutbox{}).Where("id = ?", row.ID).
		Update("created_at", time.Now().UTC().Add(-time.Minute))
	rec.mu.Lock()
	rec.fail = false
	rec.mu.Unlock()

	svc.RelayOutboxOnce(context.Background())

	if rec.count() != 1 {
		t.Fatalf("relay should have re-published exactly once, got %d", rec.count())
	}
	if err := svc.db.First(&row, "id = ?", row.ID).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if row.PublishedAt == nil {
		t.Fatalf("relay must stamp published_at on success")
	}
	if raw, ok := rec.raws[0].(json.RawMessage); !ok || len(raw) == 0 {
		t.Fatalf("relay must republish the original bytes as json.RawMessage, got %T", rec.raws[0])
	}
}

func TestOutbox_RelaySkipsFreshAndExhaustedRows(t *testing.T) {
	rec := &outboxRecorder{}
	svc := newOutboxTestService(t, rec)

	fresh := EventOutbox{ID: uuid.New(), EventName: "a.b.created", Payload: "{}", CreatedAt: time.Now().UTC()}
	exhausted := EventOutbox{ID: uuid.New(), EventName: "a.b.updated", Payload: "{}",
		CreatedAt: time.Now().UTC().Add(-time.Hour), Attempts: outboxMaxAttempts}
	if err := svc.db.Create(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.db.Create(&exhausted).Error; err != nil {
		t.Fatal(err)
	}

	svc.RelayOutboxOnce(context.Background())

	if rec.count() != 0 {
		t.Fatalf("relay must not touch fresh (< min-age) or attempt-exhausted rows; published %d", rec.count())
	}
}
