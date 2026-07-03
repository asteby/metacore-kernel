package dynamic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// TestApplyTransitionSets_Scalar asserts a scalar `set` value is assigned to the
// record and reported as a change.
func TestApplyTransitionSets_Scalar(t *testing.T) {
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "open"}, {Key: "testing"}},
		OnTransition: []manifest.TransitionHookDef{
			{From: "*", To: "testing", Set: map[string]any{"reference": "AUTO"}},
		},
	}
	rec := map[string]any{"reference": "old"}
	if !ApplyTransitionSets(sm, "open", "testing", rec) {
		t.Fatal("expected changed=true for scalar set")
	}
	if rec["reference"] != "AUTO" {
		t.Fatalf("reference = %v, want AUTO", rec["reference"])
	}
}

// TestApplyTransitionSets_AppendIdempotent asserts a "+tag" appends to a json
// array column and is idempotent (a second apply over the same value no-ops).
func TestApplyTransitionSets_AppendIdempotent(t *testing.T) {
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "open"}, {Key: "testing"}},
		OnTransition: []manifest.TransitionHookDef{
			{From: "*", To: "testing", Set: map[string]any{"labels": "+needs-testing"}},
		},
	}
	rec := map[string]any{"labels": []any{"urgent"}}
	if !ApplyTransitionSets(sm, "open", "testing", rec) {
		t.Fatal("expected changed=true on first append")
	}
	got, _ := asStringSlice(rec["labels"])
	if len(got) != 2 || got[0] != "urgent" || got[1] != "needs-testing" {
		t.Fatalf("labels = %v, want [urgent needs-testing]", got)
	}
	// Second apply over the now-present tag is a no-op.
	if ApplyTransitionSets(sm, "open", "testing", rec) {
		t.Fatal("expected changed=false on idempotent re-append")
	}
}

// TestApplyTransitionSets_AppendToAbsentAndRawJSON asserts an append works over an
// absent column (creates the array) and over a raw-JSON-bytes value (how a
// persisted jsonb column round-trips through toMap).
func TestApplyTransitionSets_AppendToAbsentAndRawJSON(t *testing.T) {
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "open"}, {Key: "testing"}},
		OnTransition: []manifest.TransitionHookDef{
			{From: "*", To: "testing", Set: map[string]any{"labels": "+a"}},
		},
	}
	// Absent column.
	rec := map[string]any{}
	if !ApplyTransitionSets(sm, "open", "testing", rec) {
		t.Fatal("expected changed=true appending to absent column")
	}
	if got, _ := asStringSlice(rec["labels"]); len(got) != 1 || got[0] != "a" {
		t.Fatalf("labels = %v, want [a]", got)
	}
	// Raw JSON bytes.
	rec2 := map[string]any{"labels": json.RawMessage(`["x"]`)}
	if !ApplyTransitionSets(sm, "open", "testing", rec2) {
		t.Fatal("expected changed=true appending to raw-json column")
	}
	if got, _ := asStringSlice(rec2["labels"]); len(got) != 2 || got[1] != "a" {
		t.Fatalf("labels = %v, want [x a]", got)
	}
}

// TestApplyTransitionSets_Remove asserts a "-tag" removes from a json array
// column and no-ops when the tag is absent.
func TestApplyTransitionSets_Remove(t *testing.T) {
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "open"}, {Key: "done"}},
		OnTransition: []manifest.TransitionHookDef{
			{From: "*", To: "done", Set: map[string]any{"labels": "-needs-testing"}},
		},
	}
	rec := map[string]any{"labels": []any{"needs-testing", "keep"}}
	if !ApplyTransitionSets(sm, "open", "done", rec) {
		t.Fatal("expected changed=true removing present tag")
	}
	got, _ := asStringSlice(rec["labels"])
	if len(got) != 1 || got[0] != "keep" {
		t.Fatalf("labels = %v, want [keep]", got)
	}
	// Removing an absent tag is a no-op.
	rec2 := map[string]any{"labels": []any{"keep"}}
	if ApplyTransitionSets(sm, "open", "done", rec2) {
		t.Fatal("expected changed=false removing absent tag")
	}
}

// TestApplyTransitionSets_NoMatch asserts a hook whose (from,to) does not match
// leaves the record untouched.
func TestApplyTransitionSets_NoMatch(t *testing.T) {
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "open"}, {Key: "testing"}, {Key: "done"}},
		OnTransition: []manifest.TransitionHookDef{
			{From: "*", To: "testing", Set: map[string]any{"reference": "AUTO"}},
		},
	}
	rec := map[string]any{"reference": "old"}
	if ApplyTransitionSets(sm, "open", "done", rec) {
		t.Fatal("expected changed=false for non-matching move")
	}
	if rec["reference"] != "old" {
		t.Fatalf("reference mutated to %v on non-match", rec["reference"])
	}
}

// capturePublisher records the last canonical event payload published.
type capturePublisher struct{ last *CanonicalEvent }

func (c *capturePublisher) Publish(_ context.Context, _, _ string, _ uuid.UUID, payload any) error {
	if ev, ok := payload.(*CanonicalEvent); ok {
		c.last = ev
	}
	return nil
}

// TestUpdate_SetPersistsAndSurfacesInEvent asserts a declarative `set` on a
// transition is persisted in the same Save AND appears in the canonical
// Issue.updated event's `after` (so wasm subscribers observe it).
func TestUpdate_SetPersistsAndSurfacesInEvent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS test_orders (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		reference TEXT,
		status TEXT
	)`)
	modelbase.Register("test_orders", func() modelbase.ModelDefiner { return &TestOrder{} })

	sm := &StageMachine{
		Field: "status",
		Stages: []manifest.StageDef{
			{Key: "backlog"}, {Key: "in_progress"},
		},
		Transitions: []manifest.TransitionDef{{From: "backlog", To: "in_progress"}},
		OnTransition: []manifest.TransitionHookDef{
			{From: "*", To: "in_progress", Set: map[string]any{"reference": "STAMPED"}},
		},
	}
	pub := &capturePublisher{}
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	svc := New(Config{
		DB:       db,
		Metadata: meta,
		Bus:      pub,
		StageMachineResolver: func(_ context.Context, model string) (*StageMachine, bool) {
			if model == "test_orders" {
				return sm, true
			}
			return nil, false
		},
	})
	user := newUser(uuid.New())
	created, err := svc.Create(context.Background(), "test_orders", user, map[string]any{
		"reference": "ORD-1", "status": "backlog",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := uuid.MustParse(created["id"].(string))

	after, err := svc.Update(context.Background(), "test_orders", user, id, map[string]any{"status": "in_progress"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if after["reference"] != "STAMPED" {
		t.Fatalf("returned after.reference = %v, want STAMPED", after["reference"])
	}
	// Persisted in the same write.
	var ref string
	db.Raw(`SELECT reference FROM test_orders WHERE id = ?`, id.String()).Scan(&ref)
	if ref != "STAMPED" {
		t.Fatalf("persisted reference = %q, want STAMPED", ref)
	}
	// Present in the canonical event's after.
	if pub.last == nil {
		t.Fatal("no canonical event published")
	}
	if pub.last.After["reference"] != "STAMPED" {
		t.Fatalf("event after.reference = %v, want STAMPED", pub.last.After["reference"])
	}
}
