package nkevents_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/nativekit"
	"github.com/asteby/metacore-kernel/nativekit/nkevents"
	"github.com/asteby/metacore-kernel/nativekit/nktest"
)

type bus struct {
	mu    sync.Mutex
	names []string
}

func (b *bus) Publish(_ context.Context, _ string, event string, _ uuid.UUID, _ any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.names = append(b.names, event)
	return nil
}

func (b *bus) got() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.names...)
}

func setup(t *testing.T, db *gorm.DB) (*nkevents.Emitter, *bus) {
	t.Helper()
	b := &bus{}
	em := nkevents.New(dynamic.NewOutboxPublisher(db, b), "demo")
	if !em.Arm() {
		t.Fatal("outbox must arm")
	}
	return em, b
}

func rows(t *testing.T, db *gorm.DB) []dynamic.EventOutbox {
	t.Helper()
	var r []dynamic.EventOutbox
	if err := db.Order("sequence").Find(&r).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEmitTxPublishesAfterCommitInOrder(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		em, b := setup(t, db)
		org := uuid.New()
		err := nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			if err := em.EmitTx(op,
				nkevents.Event{Org: org, Name: "demo.thing.created", Payload: map[string]any{"n": 1}, AggregateType: "thing", AggregateID: "a"},
				nkevents.Event{Org: org, Name: "demo.thing.updated", Payload: map[string]any{"n": 2}, AggregateType: "thing", AggregateID: "a"},
			); err != nil {
				return err
			}
			if len(b.got()) != 0 {
				t.Error("nothing may be published before commit")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := b.got(); len(got) != 2 || got[0] != "demo.thing.created" || got[1] != "demo.thing.updated" {
			t.Fatalf("published %v", got)
		}
		r := rows(t, db)
		if len(r) != 2 || r[0].Sequence >= r[1].Sequence {
			t.Fatalf("rows must carry an increasing aggregate sequence: %+v", r)
		}
		if r[0].PublishedAt == nil {
			t.Fatal("published rows are stamped")
		}
	})
}

func TestEmitTxRollbackLeavesNothing(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		em, b := setup(t, db)
		boom := errors.New("later failure")
		err := nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			_ = em.EmitTx(op, nkevents.Event{Org: uuid.New(), Name: "demo.x", Payload: 1, AggregateType: "t", AggregateID: "1"})
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if len(b.got()) != 0 || len(rows(t, db)) != 0 {
			t.Fatalf("rolled back operation must not persist or publish: bus=%v rows=%d", b.got(), len(rows(t, db)))
		}
	})
}

func TestEmitTxRejectsBadInput(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		em, _ := setup(t, db)
		_ = nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			if err := em.EmitTx(op, nkevents.Event{Org: uuid.New()}); !nativekit.HasCode(err, nativekit.CodeInvalid) {
				t.Errorf("empty name: %v", err)
			}
			if err := em.EmitTx(op, nkevents.Event{Org: uuid.New(), Name: "x", Payload: make(chan int)}); !nativekit.HasCode(err, nativekit.CodeInvalid) {
				t.Errorf("bad payload: %v", err)
			}
			return nil
		})
	})
}
