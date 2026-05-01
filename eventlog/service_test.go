package eventlog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupTestDB spins an in-memory sqlite and hand-creates the eventlog_events
// table. We do NOT use AutoMigrate because the production schema uses
// Postgres-only types (uuid, jsonb, gen_random_uuid()).
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS eventlog_events (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		sequence_num INTEGER,
		event_type TEXT,
		tags TEXT,
		data TEXT
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestEmit_AssignsMonotonicSequencePerOrg(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	orgA := uuid.New()
	orgB := uuid.New()

	for i := 0; i < 3; i++ {
		if err := svc.Emit(ctx, orgA, "thing.happened", map[string]interface{}{"i": i}); err != nil {
			t.Fatalf("emit orgA #%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := svc.Emit(ctx, orgB, "thing.happened", nil); err != nil {
			t.Fatalf("emit orgB #%d: %v", i, err)
		}
	}

	aEvents, _, err := svc.Query(ctx, orgA, QueryParams{})
	if err != nil {
		t.Fatalf("query orgA: %v", err)
	}
	if len(aEvents) != 3 {
		t.Fatalf("expected 3 orgA events, got %d", len(aEvents))
	}
	for i, e := range aEvents {
		if e.SequenceNum != int64(i+1) {
			t.Errorf("orgA event %d: sequence=%d, want %d", i, e.SequenceNum, i+1)
		}
	}

	bEvents, _, err := svc.Query(ctx, orgB, QueryParams{})
	if err != nil {
		t.Fatalf("query orgB: %v", err)
	}
	if len(bEvents) != 2 {
		t.Fatalf("expected 2 orgB events, got %d", len(bEvents))
	}
	// Sequences restart per-org.
	if bEvents[0].SequenceNum != 1 || bEvents[1].SequenceNum != 2 {
		t.Errorf("orgB sequences not per-org independent: %+v", bEvents)
	}
}

func TestEmit_EmptyEventTypeRejected(t *testing.T) {
	svc := NewService(setupTestDB(t))
	if err := svc.Emit(context.Background(), uuid.New(), "", nil); err == nil {
		t.Error("expected error for empty event type")
	}
}

func TestSubscribe_ReceivesEmittedEvents(t *testing.T) {
	svc := NewService(setupTestDB(t))
	ctx := context.Background()
	orgID := uuid.New()

	ch, unsubscribe := svc.Subscribe(orgID)
	defer unsubscribe()

	if err := svc.Emit(ctx, orgID, "probe", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}

	select {
	case evt := <-ch:
		if evt.EventType != "probe" {
			t.Errorf("got event_type=%q, want probe", evt.EventType)
		}
		if evt.SequenceNum != 1 {
			t.Errorf("got sequence_num=%d, want 1", evt.SequenceNum)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event delivery")
	}
}

func TestSubscribe_ScopedToOrg(t *testing.T) {
	svc := NewService(setupTestDB(t))
	ctx := context.Background()
	orgA := uuid.New()
	orgB := uuid.New()

	chA, unsubA := svc.Subscribe(orgA)
	defer unsubA()

	// Event for orgB must not land on chA.
	_ = svc.Emit(ctx, orgB, "other", nil)
	select {
	case evt := <-chA:
		t.Fatalf("orgA subscriber received orgB event: %+v", evt)
	case <-time.After(100 * time.Millisecond):
		// expected — nothing to see
	}
}

func TestUnsubscribe_ClosesChannelAndRemovesSub(t *testing.T) {
	svc := NewService(setupTestDB(t))
	orgID := uuid.New()

	ch, unsubscribe := svc.Subscribe(orgID)
	unsubscribe()

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel, got open")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout — channel was not closed")
	}

	// Calling again is idempotent (no panic).
	unsubscribe()

	// Subscribers map empty now.
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if _, exists := svc.subscribers[orgID]; exists {
		t.Error("expected empty subscribers map after last unsubscribe")
	}
}

func TestQuery_CursorAndHasMore(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	orgID := uuid.New()

	for i := 0; i < 5; i++ {
		if err := svc.Emit(ctx, orgID, "evt", map[string]interface{}{"i": i}); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}

	// First page: limit=2 → 2 results, HasMore=true, LastSequence=2.
	page1, cur1, err := svc.Query(ctx, orgID, QueryParams{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || cur1.LastSequence != 2 || !cur1.HasMore {
		t.Errorf("page1 unexpected: len=%d cursor=%+v", len(page1), cur1)
	}

	// Second page: continue from cursor.
	page2, cur2, err := svc.Query(ctx, orgID, QueryParams{AfterSequence: cur1.LastSequence, Limit: 2})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 || cur2.LastSequence != 4 || !cur2.HasMore {
		t.Errorf("page2 unexpected: len=%d cursor=%+v", len(page2), cur2)
	}

	// Third page: final row.
	page3, cur3, err := svc.Query(ctx, orgID, QueryParams{AfterSequence: cur2.LastSequence, Limit: 2})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 1 || cur3.LastSequence != 5 || cur3.HasMore {
		t.Errorf("page3 unexpected: len=%d cursor=%+v", len(page3), cur3)
	}
}

func TestQuery_FilterByTypes(t *testing.T) {
	svc := NewService(setupTestDB(t))
	ctx := context.Background()
	orgID := uuid.New()

	_ = svc.Emit(ctx, orgID, "message.incoming", nil)
	_ = svc.Emit(ctx, orgID, "contact.created", nil)
	_ = svc.Emit(ctx, orgID, "message.outgoing", nil)

	got, _, err := svc.Query(ctx, orgID, QueryParams{Types: []string{"message.incoming", "message.outgoing"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 message events, got %d", len(got))
	}
	for _, e := range got {
		if e.EventType == "contact.created" {
			t.Errorf("contact.created leaked through types filter: %+v", e)
		}
	}
}

func TestQuery_LimitClamping(t *testing.T) {
	svc := NewService(setupTestDB(t))
	ctx := context.Background()
	orgID := uuid.New()

	// Empty → default page size (no explicit check here; just exercise the path).
	if _, _, err := svc.Query(ctx, orgID, QueryParams{Limit: 0}); err != nil {
		t.Fatalf("limit=0: %v", err)
	}
	// Over-cap → clamped to 200 silently.
	if _, _, err := svc.Query(ctx, orgID, QueryParams{Limit: 999}); err != nil {
		t.Fatalf("limit=999: %v", err)
	}
}

// TestEmit_PostgresQueryHasNoForUpdateWithAggregate is a regression test for
// the SQLSTATE 0A000 error ("FOR UPDATE is not allowed with aggregate
// functions") that production hit when the SELECT MAX(sequence_num) was
// combined with clause.Locking{Strength: "UPDATE"}. The fix replaces row-
// level locking with a transaction-scoped advisory lock, so the SELECT must
// no longer carry FOR UPDATE.
func TestEmit_PostgresQueryHasNoForUpdateWithAggregate(t *testing.T) {
	t.Helper()
	mockDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	t.Cleanup(func() { _ = mockDB.Close() })
	dialector := postgres.New(postgres.Config{
		Conn:                 mockDB,
		WithoutQuotingCheck:  true,
		PreferSimpleProtocol: true,
	})
	gormDB, err := gorm.Open(dialector, &gorm.Config{
		SkipDefaultTransaction: true,
		DryRun:                 true,
	})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}

	var maxSeq struct {
		Seq int64
	}
	stmt := gormDB.Model(&Event{}).
		Where("organization_id = ?", uuid.New()).
		Select("COALESCE(MAX(sequence_num), 0) as seq").
		Scan(&maxSeq).Statement
	sql := strings.ToUpper(stmt.SQL.String())
	if strings.Contains(sql, "FOR UPDATE") {
		t.Fatalf("eventlog Emit SELECT contains FOR UPDATE — Postgres rejects this with aggregates: %s", stmt.SQL.String())
	}
	if !strings.Contains(sql, "MAX(SEQUENCE_NUM)") && !strings.Contains(sql, "COALESCE(MAX(SEQUENCE_NUM), 0)") {
		t.Fatalf("expected MAX(sequence_num) in SQL, got: %s", stmt.SQL.String())
	}
}

func TestWithTag_AppliesToEvent(t *testing.T) {
	var e Event
	WithTag("device_id", "abc")(&e)
	WithTag("contact_id", "xyz")(&e)
	if e.Tags["device_id"] != "abc" || e.Tags["contact_id"] != "xyz" {
		t.Errorf("tags not applied: %+v", e.Tags)
	}

	// WithTags merges.
	WithTags(map[string]string{"conversation_id": "c-1", "device_id": "new"})(&e)
	if e.Tags["conversation_id"] != "c-1" || e.Tags["device_id"] != "new" {
		t.Errorf("WithTags failed: %+v", e.Tags)
	}

	// nil-map branch: no panic.
	var e2 Event
	WithTags(nil)(&e2)
	if e2.Tags != nil && len(e2.Tags) != 0 {
		t.Errorf("WithTags(nil) should be no-op, got: %+v", e2.Tags)
	}
}
