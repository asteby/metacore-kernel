package agent

import (
	"context"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGormProposalStoreSurvivesRestart(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateProposals(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateProposals(db); err != nil {
		t.Fatal("migration must be idempotent:", err)
	}
	org, user, sess := uuid.New(), uuid.New(), uuid.New()
	mk := func(off time.Duration) *MutationProposal {
		return &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, SessionID: sess, Action: "update",
			Model: "orders", Data: map[string]any{"n": 1.0}, Status: "pending", CreatedAt: time.Now().Add(off)}
	}
	a, b := mk(-time.Minute), mk(0)
	NewGormProposalStore(db).Put(b)
	NewGormProposalStore(db).Put(a)

	s := NewGormProposalStore(db) // a "restarted" process: fresh store, same DB
	list := s.ListPending(sess, user)
	if len(list) != 2 || list[0].ID != a.ID {
		t.Fatalf("pending = %+v", list)
	}
	if got, ok := s.Get(a.ID); !ok || got.Model != "orders" || got.Data["n"] != 1.0 {
		t.Fatalf("get = %+v %v", got, ok)
	}
	if p, ok := s.SetStatus(a.ID, "approved"); !ok || p.Status != "approved" {
		t.Fatalf("set status = %+v %v", p, ok)
	}
	if _, ok := s.SetStatus(uuid.New(), "approved"); ok {
		t.Fatal("unknown id must report not found")
	}
	if list := s.ListPending(sess, user); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("pending after approve = %+v", list)
	}
	if list := s.ListPending(sess, uuid.New()); len(list) != 0 {
		t.Fatal("another user must not see the proposals")
	}
}

func TestGormToolTraceStoreHook(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateToolTraces(db); err != nil {
		t.Fatal(err)
	}
	st := NewGormToolTraceStore(db, 8)
	hook := st.Hook()
	org := uuid.New()
	hook(context.Background(), &contextmgr.ConversationContext{OrganizationID: org}, ToolTrace{Tool: "query_data", Success: true, DurationMS: 12, Attempts: 1})
	hook(context.Background(), nil, ToolTrace{Tool: "x", Error: "boom"})
	st.Close() // flushes
	var rows []toolTraceRow
	if err := db.Order("tool").Find(&rows).Error; err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d err = %v", len(rows), err)
	}
	if rows[0].Tool != "query_data" || rows[0].OrganizationID != org || rows[0].DurationMS != 12 || !rows[0].Success {
		t.Fatalf("row = %+v", rows[0])
	}
}
