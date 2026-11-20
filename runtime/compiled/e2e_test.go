package compiled_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/runtime/compiled"
	"github.com/asteby/metacore-kernel/runtime/wasm"
	"github.com/asteby/metacore-kernel/security"
)

type Widget struct {
	modelbase.BaseUUIDModel
	Name string `json:"name" gorm:"size:255"`
}

func (Widget) TableName() string { return "e2e_widgets" }
func (Widget) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "W", Columns: []modelbase.ColumnDef{{Key: "name", Label: "Name"}}}
}
func (Widget) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "W"} }

type usr struct{ id, org uuid.UUID }

func (u usr) GetID() uuid.UUID             { return u.id }
func (u usr) GetOrganizationID() uuid.UUID { return u.org }
func (u usr) GetEmail() string             { return "" }
func (u usr) GetRole() string              { return "" }
func (u usr) GetPasswordHash() string      { return "" }
func (u usr) SetEmail(string)              {}
func (u usr) SetName(string)               {}
func (u usr) SetPasswordHash(string)       {}
func (u usr) SetRole(string)               {}
func (u usr) SetOrganizationID(uuid.UUID)  {}

// A compiled handler goes through Service.ExecAction, so the kernel's action
// gates (idempotency, approval) apply to it exactly as to any other trigger,
// and a declared symbol nothing implements fails instead of answering OK.
func TestE2E_CompiledAction_Gates(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE e2e_widgets (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, name TEXT)`)
	if err := db.AutoMigrate(&dynamic.ApprovalRequest{}); err != nil {
		t.Fatal(err)
	}
	modelbase.Register("e2e_widgets", func() modelbase.ModelDefiner { return &Widget{} })

	ctx := context.Background()
	host, err := wasm.NewHost(ctx, security.Compile("modx", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	var ran atomic.Int32
	if err := host.LoadNative("modx", map[string]wasm.NativeFunc{
		"Capture": func(context.Context, *wasm.NativeCall, []byte) ([]byte, error) {
			ran.Add(1)
			return []byte(`{"success":true,"data":{"charge":"ch_1"}}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	trig := &manifest.ActionTrigger{Type: "wasm", Export: "Capture"}
	defs := map[string]*manifest.ActionDef{
		"capture": {Key: "capture", Trigger: trig, Idempotency: &manifest.IdempotencyDef{KeyField: "request_id"}},
		"approve": {Key: "approve", Trigger: trig, Approval: &manifest.ApprovalDef{Roles: []string{"admin"}}},
		"missing": {Key: "missing", Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "NotImplemented"}},
	}
	d := compiled.New(host, nil)
	svc := dynamic.New(dynamic.Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		ActionResolver: func(_ context.Context, _ string, key string) (*manifest.ActionDef, bool) {
			def, ok := defs[key]
			return def, ok
		},
		ActionDispatchers: map[string]dynamic.ActionDispatcher{
			"wasm": d.Actions(func(context.Context, string) string { return "modx" }),
		},
	})
	u := usr{uuid.New(), uuid.New()}
	row, err := svc.Create(ctx, "e2e_widgets", u, map[string]any{"name": "w"})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(row["id"].(string))

	for i := 0; i < 2; i++ {
		res, err := svc.ExecAction(ctx, "e2e_widgets", u, id, "capture", map[string]any{"request_id": "r-1"})
		if err != nil || !res.Success {
			t.Fatalf("call %d: %+v %v", i, res, err)
		}
	}
	if ran.Load() != 1 {
		t.Fatalf("idempotent replay must not re-run the compiled handler: ran=%d", ran.Load())
	}

	if res, err := svc.ExecAction(ctx, "e2e_widgets", u, id, "missing", nil); err == nil {
		t.Fatalf("a missing compiled export must fail, not answer OK: %+v", res)
	}

	before := ran.Load()
	_, err = svc.ExecAction(ctx, "e2e_widgets", u, id, "approve", nil)
	var need *dynamic.ApprovalRequiredError
	if !errors.As(err, &need) {
		t.Fatalf("approval policy must park the action, got %v", err)
	}
	if ran.Load() != before {
		t.Fatal("a parked action must not run the handler")
	}
}
