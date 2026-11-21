package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

func transitionBusFixture(t *testing.T, emit bool) (*Service, *fanOutBus, *fakeUser) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS test_orders (
		id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
		reference TEXT, status TEXT)`)
	modelbase.Register("test_orders", func() modelbase.ModelDefiner { return &TestOrder{} })
	sm := &StageMachine{
		Field:       "status",
		Stages:      []manifest.StageDef{{Key: "draft"}, {Key: "confirmed"}},
		Transitions: []manifest.TransitionDef{{From: "draft", To: "confirmed"}},
	}
	bus := newFanOutBus()
	svc := New(Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		Bus:                          bus,
		EmitTransitionEvents:         emit,
		StageMachineResolver: func(_ context.Context, model string) (*StageMachine, bool) {
			return sm, model == "test_orders"
		},
	})
	return svc, bus, newUser(uuid.New())
}

func (b *fanOutBus) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, p := range b.published {
		out = append(out, p.event)
	}
	return out
}

func TestTransitionEvent_PublishedWhenEnabled(t *testing.T) {
	svc, bus, u := transitionBusFixture(t, true)
	out, err := svc.Create(context.Background(), "test_orders", u, map[string]any{"reference": "R", "status": "draft"})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(out["id"].(string))
	if _, err := svc.Update(context.Background(), "test_orders", u, id, map[string]any{"status": "confirmed"}); err != nil {
		t.Fatal(err)
	}
	var found *CanonicalEvent
	for _, p := range bus.published {
		if ev, ok := p.payload.(*CanonicalEvent); ok && ev.Action == "transitioned" {
			found = ev
		}
	}
	if found == nil {
		t.Fatalf("no transitioned event among %v", bus.names())
	}
	if found.StageFrom != "draft" || found.StageTo != "confirmed" {
		t.Fatalf("from/to = %q/%q", found.StageFrom, found.StageTo)
	}
	// A plain edit that does not move the stage emits no transition.
	n := len(bus.published)
	if _, err := svc.Update(context.Background(), "test_orders", u, id, map[string]any{"reference": "R2"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range bus.published[n:] {
		if ev, ok := p.payload.(*CanonicalEvent); ok && ev.Action == "transitioned" {
			t.Fatal("a non-moving update must not emit transitioned")
		}
	}
}

func TestTransitionEvent_OffByDefault(t *testing.T) {
	svc, bus, u := transitionBusFixture(t, false)
	out, _ := svc.Create(context.Background(), "test_orders", u, map[string]any{"reference": "R", "status": "draft"})
	id := uuid.MustParse(out["id"].(string))
	if _, err := svc.Update(context.Background(), "test_orders", u, id, map[string]any{"status": "confirmed"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range bus.published {
		if ev, ok := p.payload.(*CanonicalEvent); ok && ev.Action == "transitioned" {
			t.Fatal("no transitioned event unless EmitTransitionEvents")
		}
	}
}
