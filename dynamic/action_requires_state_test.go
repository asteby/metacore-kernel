package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// TestOrder is a model with a `status` column so we can exercise the
// requires_state gate ExecAction enforces. It mirrors the TestProduct harness
// (BaseUUIDModel + TableName + DefineTable) but adds the lifecycle column.
type TestOrder struct {
	modelbase.BaseUUIDModel
	Reference string `json:"reference" gorm:"size:255"`
	Status    string `json:"status" gorm:"size:64"`
}

func (TestOrder) TableName() string { return "test_orders" }
func (TestOrder) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Test Orders",
		Columns: []modelbase.ColumnDef{
			{Key: "reference", Label: "Reference", Sortable: true},
			{Key: "status", Label: "Status", Sortable: true},
		},
		SearchColumns: []string{"reference"},
	}
}
func (TestOrder) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Test Order"}
}

// setupOrderFixture wires a service + handler over a test_orders table, mirroring
// setupActionFixture but for a model that carries a status column.
func setupOrderFixture(t *testing.T) (*actionFixture, uuid.UUID) {
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

	wasm := &stubDispatcher{}
	webhook := &stubDispatcher{}
	fx := &actionFixture{
		db:      db,
		wasm:    wasm,
		webhook: webhook,
		actions: map[string]map[string]*manifest.ActionDef{},
	}

	meta := metadata.New(metadata.Config{CacheTTL: -1})
	fx.svc = New(Config{
		DB:       db,
		Metadata: meta,
		ActionResolver: func(_ context.Context, model, key string) (*manifest.ActionDef, bool) {
			if m, ok := fx.actions[model]; ok {
				if def, ok := m[key]; ok {
					return def, true
				}
			}
			return nil, false
		},
		ActionDispatchers: map[string]ActionDispatcher{
			"wasm":    wasm,
			"webhook": webhook,
		},
	})

	fx.user = newUser(uuid.New())
	created, err := fx.svc.Create(context.Background(), "test_orders", fx.user, map[string]any{
		"reference": "ORD-1",
		"status":    "reception",
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	id, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	h := NewHandler(fx.svc, fakeUserResolver(fx.user))
	app := fiber.New()
	h.Mount(app)
	fx.app = app
	return fx, id
}

// TestExecAction_RequiresState is table-driven over the gate: an action gated on
// a set of statuses is rejected with ErrInvalidState (HTTP 409) when the record
// is in a disallowed state, and dispatched normally when allowed. An action with
// no requires_state is unaffected (additive guarantee).
func TestExecAction_RequiresState(t *testing.T) {
	cases := []struct {
		name          string
		requiresState []string
		recordStatus  string
		wantDispatch  bool
		wantErr       error
	}{
		{
			name:          "allowed status dispatches",
			requiresState: []string{"reception", "inspecting"},
			recordStatus:  "reception",
			wantDispatch:  true,
		},
		{
			name:          "disallowed status is gated",
			requiresState: []string{"inspecting", "ready"},
			recordStatus:  "reception",
			wantDispatch:  false,
			wantErr:       ErrInvalidState,
		},
		{
			name:          "empty requires_state never gates",
			requiresState: nil,
			recordStatus:  "reception",
			wantDispatch:  true,
		},
		{
			name:          "single allowed status dispatches",
			requiresState: []string{"reception"},
			recordStatus:  "reception",
			wantDispatch:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx, id := setupOrderFixture(t)
			// Force the record's status to the case's value.
			if err := fx.db.Exec(`UPDATE test_orders SET status = ? WHERE id = ?`, tc.recordStatus, id.String()).Error; err != nil {
				t.Fatalf("set status: %v", err)
			}
			fx.registerAction("test_orders", &manifest.ActionDef{
				Key:           "advance",
				Trigger:       &manifest.ActionTrigger{Type: "wasm", Export: "advance"},
				RequiresState: tc.requiresState,
			})

			dispatched := false
			fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
				dispatched = true
				return ActionResponse{Success: true}, nil
			}

			res, err := fx.svc.ExecAction(context.Background(), "test_orders", fx.user, id, "advance", map[string]any{})

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if dispatched != tc.wantDispatch {
				t.Fatalf("dispatched = %v, want %v (res=%+v err=%v)", dispatched, tc.wantDispatch, res, err)
			}
		})
	}
}

func TestCheckRequiresState_StateColumnFallback(t *testing.T) {
	cases := []struct {
		name    string
		row     map[string]any
		allowed []string
		wantErr bool
	}{
		{
			name:    "state draft rejects receive",
			row:     map[string]any{"state": "draft"},
			allowed: []string{"confirmed", "partial"},
			wantErr: true,
		},
		{
			name:    "state confirmed allows receive",
			row:     map[string]any{"state": "confirmed"},
			allowed: []string{"confirmed", "partial"},
		},
		{
			name:    "empty status falls back to state",
			row:     map[string]any{"status": "", "state": "partial"},
			allowed: []string{"confirmed", "partial"},
		},
		{
			name:    "status wins over state",
			row:     map[string]any{"status": "reception", "state": "draft"},
			allowed: []string{"reception"},
		},
		{
			name:    "status wins when disallowed even if state allowed",
			row:     map[string]any{"status": "shipped", "state": "confirmed"},
			allowed: []string{"confirmed"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRequiresState(tc.row, tc.allowed)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidState) {
					t.Fatalf("err = %v, want ErrInvalidState", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}

// TestExecAction_RequiresState_HTTP409 confirms the gate maps to HTTP 409 through
// the handler error path (not the 422 a guest-declined dispatch produces).
func TestExecAction_RequiresState_HTTP409(t *testing.T) {
	fx, id := setupOrderFixture(t)
	if err := fx.db.Exec(`UPDATE test_orders SET status = ? WHERE id = ?`, "shipped", id.String()).Error; err != nil {
		t.Fatalf("set status: %v", err)
	}
	fx.registerAction("test_orders", &manifest.ActionDef{
		Key:           "receive",
		Trigger:       &manifest.ActionTrigger{Type: "wasm", Export: "receive"},
		RequiresState: []string{"reception"},
	})
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		t.Fatalf("dispatcher must not run when state gate fails")
		return ActionResponse{}, nil
	}

	status, env := invokeAction(t, fx.app, "test_orders", id, "receive", `{}`)
	if status != fiber.StatusConflict {
		t.Fatalf("status = %d, want 409; env=%v", status, env)
	}
	if env["success"] != false {
		t.Fatalf("success = %v, want false", env["success"])
	}
}
