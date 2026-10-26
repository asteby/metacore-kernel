package wasm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func (x *commitHarness) invokeTx(t *testing.T, ctx context.Context, tx *gorm.DB, org uuid.UUID) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := x.h.InvokeForInTx(ctx, tx, org, uuid.New(), "testaddon", "emit_test", []byte(aggPayload), nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HANG: emit inside the caller's tx blocked")
	}
}

// Rollback of the caller's tx: nothing is delivered, no row survives, and the
// aggregate sequence the rolled-back event consumed is not burnt.
func TestEmitInTx_RollbackDeliversNothingAndSequenceIsNotRepeated(t *testing.T) {
	x := newCommitHarness(t, []byte(`{"success":true}`), nil)
	org := uuid.New()
	ctx, hook := WithCommitHook(context.Background())
	tx := x.db.Begin()
	x.invokeTx(t, ctx, tx, org)
	if n := x.fired.Load(); n != 0 {
		t.Fatalf("published %d event(s) before commit", n)
	}
	tx.Rollback()
	hook.Discard()
	x.h.PublishCommitted(ctx, hook) // no-op after Discard
	if n := x.fired.Load(); n != 0 {
		t.Fatalf("rollback delivered %d event(s)", n)
	}
	if rows := x.rows(t); len(rows) != 0 {
		t.Fatalf("rollback left %d outbox row(s)", len(rows))
	}
	var seqs []dynamic.EventSequence
	x.db.Find(&seqs)
	if len(seqs) != 0 {
		t.Fatalf("rollback left sequence rows: %+v", seqs)
	}
	// The next real event is the first one of the aggregate, delivered once.
	if _, err := x.h.InvokeFor(context.Background(), org, uuid.New(), "testaddon", "emit_test", []byte(aggPayload), nil); err != nil {
		t.Fatal(err)
	}
	if x.fired.Load() != 1 || x.metas[0].Sequence != 1 {
		t.Fatalf("fired=%d metas=%+v, want one delivery with sequence 1", x.fired.Load(), x.metas)
	}
}

// Commit: rows are in the tx, nothing is published until the hook runs.
func TestEmitInTx_CommitPublishesOnlyViaHook(t *testing.T) {
	x := newCommitHarness(t, []byte(`{"success":true}`), nil)
	org := uuid.New()
	ctx, hook := WithCommitHook(context.Background())
	tx := x.db.Begin()
	x.invokeTx(t, ctx, tx, org)
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if n := x.fired.Load(); n != 0 {
		t.Fatalf("published %d event(s) at commit without the hook", n)
	}
	if rows := x.rows(t); len(rows) != 1 || rows[0].PublishedAt != nil || rows[0].Sequence != 1 {
		t.Fatalf("want one committed unpublished row (sequence 1), got %+v", rows)
	}
	x.h.PublishCommitted(ctx, hook)
	x.h.PublishCommitted(ctx, hook) // idempotent
	if n := x.fired.Load(); n != 1 {
		t.Fatalf("hook delivered %d event(s), want 1", n)
	}
	if rows := x.rows(t); rows[0].PublishedAt == nil {
		t.Fatal("hook must stamp published_at")
	}
	if x.metas[0].Sequence != 1 || x.metas[0].ID != x.rows(t)[0].EventID {
		t.Fatalf("delivered identity differs from the row: %+v", x.metas[0])
	}
}

// Without a hook the committed row waits for the relay: never lost, never in line.
func TestEmitInTx_NoHookLeavesCommittedRowForRelay(t *testing.T) {
	x := newCommitHarness(t, []byte(`{"success":true}`), nil)
	tx := x.db.Begin()
	x.invokeTx(t, context.Background(), tx, uuid.New())
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if x.fired.Load() != 0 {
		t.Fatal("published in line without a commit hook")
	}
	if rows := x.rows(t); len(rows) != 1 || rows[0].PublishedAt != nil {
		t.Fatalf("want one unpublished committed row, got %+v", rows)
	}
}

// An unarmed publisher inside a single-connection tx must not hang (the arm
// migration used to run on the hot path while the tx held the only conn) and
// must still deliver after commit (non-durably).
func TestEmitInTx_UnarmedPublisherDoesNotHang(t *testing.T) {
	x := newCommitHarness(t, []byte(`{"success":true}`), nil)
	x.h.pubMu.Lock()
	x.h.pub, x.h.pubAuto = dynamic.NewOutboxPublisher(x.db, x.h.bus), false // NOT armed
	x.h.pubMu.Unlock()
	ctx, hook := WithCommitHook(context.Background())
	tx := x.db.Begin()
	x.invokeTx(t, ctx, tx, uuid.New())
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	x.h.PublishCommitted(ctx, hook)
	if x.fired.Load() != 1 {
		t.Fatalf("fired=%d, want 1 (non-durable publish after commit)", x.fired.Load())
	}
}

// The host arms the outbox when it is wired, not on the first emit.
func TestWiring_ArmsOutboxEagerly(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:arm_%s?mode=memory&cache=shared", uuid.NewString())),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	h, err := NewHost(context.Background(), security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	h.WithBus(events.NewBus(nil)).WithDB(db)
	if !db.Migrator().HasTable(&dynamic.EventOutbox{}) || !db.Migrator().HasTable(&dynamic.EventSequence{}) {
		t.Fatal("outbox tables must exist right after WithBus+WithDB")
	}

	// WithDurableEmits(false): no tables, no outbox.
	db2, _ := gorm.Open(sqlite.Open(fmt.Sprintf("file:arm2_%s?mode=memory&cache=shared", uuid.NewString())),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	h2, _ := NewHost(context.Background(), security.Compile("testaddon", nil), nil)
	defer h2.Close(context.Background())
	h2.WithDurableEmits(false).WithBus(events.NewBus(nil)).WithDB(db2)
	if db2.Migrator().HasTable(&dynamic.EventOutbox{}) {
		t.Fatal("WithDurableEmits(false) must not create the outbox")
	}
}

func TestStageEmit_AggregateValidation(t *testing.T) {
	ctx := context.Background()
	org := uuid.New()
	long := strings.Repeat("a", 129)
	bad := []string{
		`{"aggregate":{"type":"Sales Order","id":"1"}}`,
		`{"aggregate":{"type":"T","id":"` + long + `"}}`,
		`{"aggregate":{"type":"` + long + `","id":"1"}}`,
		`{"aggregate":{"type":"T"}}`,
		`{"aggregate":{"id":"1"}}`,
		`{"aggregate":{"type":"T","id":"a/b"}}`,
		`{"aggregate":"x"}`,
		`{"aggregate":{"type":"T\n","id":"1"}}`,
	}
	for _, p := range bad {
		if _, err := stageEmit(ctx, "a", "a.ev", org, []byte(p)); err == nil {
			t.Errorf("payload %.60s accepted, want error", p)
		}
	}
	good := []string{
		`{"aggregate":{"type":"SalesOrder","id":"so-1.x:y_z"}}`,
		`{"aggregate":{"type":"` + strings.Repeat("a", 128) + `","id":"1"}}`,
		`{"aggregate":{}}`, `{"aggregate":null}`, `{"n":1}`, `[1,2]`, ``,
	}
	for _, p := range good {
		if _, err := stageEmit(ctx, "a", "a.ev", org, []byte(p)); err != nil {
			t.Errorf("payload %.60s rejected: %v", p, err)
		}
	}
}
