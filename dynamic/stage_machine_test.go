package dynamic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// stageFixture wires a service over test_orders whose `status` column is a stage
// machine (backlog → in_progress → review → done, with review ↔ in_progress).
type stageFixture struct {
	db   *gorm.DB
	svc  *Service
	wasm *stubDispatcher
	user *fakeUser
	id   uuid.UUID
}

func setupStageFixture(t *testing.T, hooks []manifest.TransitionHookDef) *stageFixture {
	t.Helper()
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
			{Key: "backlog", Label: "Backlog", Color: "slate", Order: 0},
			{Key: "in_progress", Label: "In Progress", Color: "blue", Order: 1},
			{Key: "review", Label: "Review", Color: "amber", Order: 2},
			{Key: "done", Label: "Done", Color: "green", Order: 3, IsFinal: true},
		},
		Transitions: []manifest.TransitionDef{
			{From: "backlog", To: "in_progress"},
			{From: "in_progress", To: "review"},
			{From: "review", To: "done"},
			{From: "review", To: "in_progress"},
		},
		OnTransition: hooks,
	}

	wasm := &stubDispatcher{}
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	svc := New(Config{
		DB:       db,
		Metadata: meta,
		StageMachineResolver: func(_ context.Context, model string) (*StageMachine, bool) {
			if model == "test_orders" {
				return sm, true
			}
			return nil, false
		},
		ActionDispatchers: map[string]ActionDispatcher{"wasm": wasm},
	})

	user := newUser(uuid.New())
	created, err := svc.Create(context.Background(), "test_orders", user, map[string]any{
		"reference": "ORD-1",
		"status":    "backlog",
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	id, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}
	return &stageFixture{db: db, svc: svc, wasm: wasm, user: user, id: id}
}

// TestUpdate_ValidTransition_Passes asserts a whitelisted move persists.
func TestUpdate_ValidTransition_Passes(t *testing.T) {
	fx := setupStageFixture(t, nil)
	after, err := fx.svc.Update(context.Background(), "test_orders", fx.user, fx.id, map[string]any{"status": "in_progress"})
	if err != nil {
		t.Fatalf("valid transition rejected: %v", err)
	}
	if after["status"] != "in_progress" {
		t.Fatalf("status = %v, want in_progress", after["status"])
	}
}

// TestUpdate_InvalidTransition_422 asserts a non-whitelisted move is rejected with
// ErrInvalidTransition and nothing is persisted.
func TestUpdate_InvalidTransition_422(t *testing.T) {
	fx := setupStageFixture(t, nil)
	// backlog → done is not a declared transition.
	_, err := fx.svc.Update(context.Background(), "test_orders", fx.user, fx.id, map[string]any{"status": "done"})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidTransition", err)
	}
	// The row must still be in its original stage.
	var status string
	fx.db.Raw(`SELECT status FROM test_orders WHERE id = ?`, fx.id.String()).Scan(&status)
	if status != "backlog" {
		t.Fatalf("status persisted to %q despite invalid transition", status)
	}
}

// TestUpdate_NoStageChange_Unaffected asserts an Update that does not touch the
// stage_field is never gated (a plain attribute edit).
func TestUpdate_NoStageChange_Unaffected(t *testing.T) {
	fx := setupStageFixture(t, nil)
	_, err := fx.svc.Update(context.Background(), "test_orders", fx.user, fx.id, map[string]any{"reference": "ORD-2"})
	if err != nil {
		t.Fatalf("non-stage update rejected: %v", err)
	}
}

// TestUpdate_HookFires asserts a matching on_transition hook is dispatched, with
// the before/after payload, after a valid move.
func TestUpdate_HookFires(t *testing.T) {
	fx := setupStageFixture(t, []manifest.TransitionHookDef{
		{From: "*", To: "in_progress", Do: "wasm:notify"},
	})
	fired := false
	fx.wasm.fn = func(_ context.Context, req ActionRequest) (ActionResponse, error) {
		fired = true
		if req.Trigger.Export != "notify" {
			t.Errorf("export = %q, want notify", req.Trigger.Export)
		}
		before, _ := req.Payload["before"].(map[string]any)
		after, _ := req.Payload["after"].(map[string]any)
		if before["status"] != "backlog" || after["status"] != "in_progress" {
			t.Errorf("payload before/after = %v / %v", before["status"], after["status"])
		}
		return ActionResponse{Success: true}, nil
	}
	if _, err := fx.svc.Update(context.Background(), "test_orders", fx.user, fx.id, map[string]any{"status": "in_progress"}); err != nil {
		t.Fatalf("update with hook: %v", err)
	}
	if !fired {
		t.Fatal("matching on_transition hook did not fire")
	}
}

// TestUpdate_HookDoesNotFireOnNonMatch asserts a hook scoped to a different
// destination stays silent.
func TestUpdate_HookDoesNotFireOnNonMatch(t *testing.T) {
	fx := setupStageFixture(t, []manifest.TransitionHookDef{
		{From: "*", To: "done", Do: "wasm:close"},
	})
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		t.Fatal("hook for to:done must not fire on backlog→in_progress")
		return ActionResponse{}, nil
	}
	if _, err := fx.svc.Update(context.Background(), "test_orders", fx.user, fx.id, map[string]any{"status": "in_progress"}); err != nil {
		t.Fatalf("update: %v", err)
	}
}

// TestUpdate_RequiredHookFailure_RollsBack asserts a failing REQUIRED hook rolls
// back the whole transition (the stage stays at its previous value).
func TestUpdate_RequiredHookFailure_RollsBack(t *testing.T) {
	fx := setupStageFixture(t, []manifest.TransitionHookDef{
		{From: "*", To: "in_progress", Do: "wasm:guard", Required: true},
	})
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		return ActionResponse{}, fmt.Errorf("boom")
	}
	_, err := fx.svc.Update(context.Background(), "test_orders", fx.user, fx.id, map[string]any{"status": "in_progress"})
	if err == nil {
		t.Fatal("expected error from required hook failure")
	}
	var status string
	fx.db.Raw(`SELECT status FROM test_orders WHERE id = ?`, fx.id.String()).Scan(&status)
	if status != "backlog" {
		t.Fatalf("status = %q after required-hook rollback, want backlog", status)
	}
}

// TestUpdate_NonRequiredHookFailure_Proceeds asserts a failing NON-required hook
// is swallowed and the transition still commits.
func TestUpdate_NonRequiredHookFailure_Proceeds(t *testing.T) {
	fx := setupStageFixture(t, []manifest.TransitionHookDef{
		{From: "*", To: "in_progress", Do: "wasm:best_effort"},
	})
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		return ActionResponse{}, fmt.Errorf("transient")
	}
	after, err := fx.svc.Update(context.Background(), "test_orders", fx.user, fx.id, map[string]any{"status": "in_progress"})
	if err != nil {
		t.Fatalf("non-required hook failure must not abort: %v", err)
	}
	if after["status"] != "in_progress" {
		t.Fatalf("status = %v, want in_progress (committed)", after["status"])
	}
}

// TestDeriveTableColumns_StageStatus asserts the stage_field column derives a
// status display with options sourced from stages[] (ordered, coloured).
func TestDeriveTableColumns_StageStatus(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "github_issues",
		ModelKey:  "Issue",
		Columns: []manifest.ColumnDef{
			{Name: "title", Type: "text"},
			{Name: "stage", Type: "text"},
		},
		StageField: "stage",
		Stages: []manifest.StageDef{
			{Key: "done", Label: "Done", Color: "green", Order: 3},
			{Key: "backlog", Label: "Backlog", Color: "slate", Order: 0},
		},
	}
	cols := DeriveTableColumns(def)
	var stage *modelbase.ColumnDef
	for i := range cols {
		if cols[i].Key == "stage" {
			stage = &cols[i]
		}
	}
	if stage == nil {
		t.Fatal("stage column missing")
	}
	if stage.CellStyle != "status" {
		t.Errorf("CellStyle = %q, want status", stage.CellStyle)
	}
	if len(stage.Options) != 2 {
		t.Fatalf("options = %d, want 2", len(stage.Options))
	}
	// Ordered by Stage.Order: backlog (0) before done (3).
	if stage.Options[0].Value != "backlog" || stage.Options[1].Value != "done" {
		t.Errorf("options order = %v, want [backlog done]", stage.Options)
	}
	if stage.Options[1].Color != "green" {
		t.Errorf("done colour = %q, want green", stage.Options[1].Color)
	}
}
