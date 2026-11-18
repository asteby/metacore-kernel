package inbound

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// record.action | record.transition | record.restore against the real
// dynamic.Service, on SQLite (CGO) and on Postgres (TEST_POSTGRES_DSN, the CI
// job "postgres").

type stageOrder struct {
	modelbase.BaseUUIDModel
	Reference string `json:"reference" gorm:"size:64"`
	Status    string `json:"status" gorm:"size:32"`
}

func (stageOrder) TableName() string { return "inbound_stage_orders" }
func (stageOrder) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Orders",
		Columns: []modelbase.ColumnDef{
			{Key: "reference", Label: "Reference"}, {Key: "status", Label: "Status"},
		},
		SearchColumns: []string{"reference"},
	}
}
func (stageOrder) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Orders"}
}

const (
	stageOrderSQLite = `CREATE TABLE inbound_stage_orders (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, reference TEXT, status TEXT)`
	stageOrderPG = `CREATE TABLE inbound_stage_orders (id uuid PRIMARY KEY, organization_id uuid, created_by_id uuid,
		created_at timestamptz, updated_at timestamptz, deleted_at timestamptz, reference text, status text)`
)

func eachStageDB(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Helper()
	run := func(t *testing.T, db *gorm.DB, ddl string) {
		t.Helper()
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
		fn(t, db)
	}
	t.Run("SQLite", func(t *testing.T) {
		db := openSQLite(t)
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.SetMaxOpenConns(4)
		}
		run(t, db, stageOrderSQLite)
	})
	t.Run("Postgres", func(t *testing.T) { run(t, postgresDB(t), stageOrderPG) })
}

func newStageExec(t *testing.T, db *gorm.DB) (*DynamicExecutor, *dynamic.Service) {
	t.Helper()
	modelbase.Register("inbound_stage_orders", func() modelbase.ModelDefiner { return &stageOrder{} })
	actions := map[string]*manifest.ActionDef{
		"mark_paid": {Key: "mark_paid", Ingest: true, RequiresState: []string{"new"}, Trigger: &manifest.ActionTrigger{Type: "noop"}},
		"capture":   {Key: "capture", Trigger: &manifest.ActionTrigger{Type: "noop"}},
		"approve": {Key: "approve", Ingest: true, Trigger: &manifest.ActionTrigger{Type: "noop"},
			Approval: &manifest.ApprovalDef{Roles: []string{"manager"}}},
	}
	sm := &dynamic.StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "new"}, {Key: "paid"}, {Key: "shipped"}},
		Transitions: []manifest.TransitionDef{
			{From: "new", To: "paid"}, {From: "paid", To: "shipped"},
		},
	}
	svc := dynamic.New(dynamic.Config{
		AllowUnconfiguredPermissions: true, DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ActionResolver: func(_ context.Context, model, key string) (*manifest.ActionDef, bool) {
			def, ok := actions[key]
			return def, ok && model == "inbound_stage_orders"
		},
		StageMachineResolver: func(_ context.Context, model string) (*dynamic.StageMachine, bool) {
			return sm, model == "inbound_stage_orders"
		},
	})
	ex := &DynamicExecutor{Service: svc, Tombstones: GormTombstones{DB: db, Table: func(string) string { return "inbound_stage_orders" }}}
	return ex, svc
}

func stageOrderOp(org uuid.UUID, kind Op, ref string) Operation {
	return Operation{Org: org, Route: Route{Model: "inbound_stage_orders", Op: kind, Match: []string{"reference"}},
		Values: map[string]any{"reference": ref}}
}

func seedStageOrder(t *testing.T, svc *dynamic.Service, org uuid.UUID, ref, status string) uuid.UUID {
	t.Helper()
	row, err := svc.Create(context.Background(), "inbound_stage_orders", dynamic.NewSystemCaller(org),
		map[string]any{"reference": ref, "status": status})
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(row["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func stageOf(t *testing.T, svc *dynamic.Service, org, id uuid.UUID) string {
	t.Helper()
	row, err := svc.Get(context.Background(), "inbound_stage_orders", dynamic.NewSystemCaller(org), id)
	if err != nil {
		t.Fatal(err)
	}
	return dynamic.StageValue(row["status"])
}

func TestDynamicRecordAction(t *testing.T) {
	eachStageDB(t, func(t *testing.T, db *gorm.DB) {
		ex, svc := newStageExec(t, db)
		ctx := context.Background()
		org, other := uuid.New(), uuid.New()
		id := seedStageOrder(t, svc, org, "R1", "new")
		seedStageOrder(t, svc, other, "R9", "new")

		op := stateOrderAction(org, "mark_paid", "R1")
		out, err := ex.Execute(ctx, op)
		if err != nil || out.Affected != 1 || out.Noop || out.RecordID != id.String() {
			t.Fatalf("action: out=%+v err=%v", out, err)
		}
		// the same action on a row that left the state it requires: a noop
		if _, err = svc.Update(ctx, "inbound_stage_orders", dynamic.NewSystemCaller(org), id, map[string]any{"status": "paid"}); err != nil {
			t.Fatal(err)
		}
		out, err = ex.Execute(ctx, op)
		if err != nil || !out.Noop || out.Reason != CodeStateNotEligible || out.Affected != 0 {
			t.Fatalf("ineligible: out=%+v err=%v", out, err)
		}
		op.OnIneligible = OnIneligibleFail
		_, err = ex.Execute(ctx, op)
		if ee := execErr(t, err); ee.Kind != KindConflict || ee.Code != CodeStateNotEligible {
			t.Fatalf("fail policy: %+v", ee)
		}
		// closed actions never run, whatever the row
		for _, key := range []string{"capture", "approve", "ghost"} {
			_, err = ex.Execute(ctx, stateOrderAction(org, key, "R1"))
			if ee := execErr(t, err); ee.Code != CodeActionNotIngestable {
				t.Fatalf("%s: %+v", key, ee)
			}
		}
		// another organization's row is invisible
		_, err = ex.Execute(ctx, stateOrderAction(org, "mark_paid", "R9"))
		if ee := execErr(t, err); ee.Kind != KindNotFound {
			t.Fatalf("other org: %+v", ee)
		}
	})
}

func stateOrderAction(org uuid.UUID, key, ref string) Operation {
	op := stageOrderOp(org, OpAction, ref)
	op.ActionKey = key
	return op
}

func TestDynamicRecordTransition(t *testing.T) {
	eachStageDB(t, func(t *testing.T, db *gorm.DB) {
		ex, svc := newStageExec(t, db)
		ctx := context.Background()
		org := uuid.New()
		id := seedStageOrder(t, svc, org, "R1", "new")

		to := func(state string) Operation {
			op := stageOrderOp(org, OpTransition, "R1")
			op.To = state
			return op
		}
		out, err := ex.Execute(ctx, to("paid"))
		if err != nil || out.Affected != 1 || stageOf(t, svc, org, id) != "paid" {
			t.Fatalf("valid: out=%+v err=%v", out, err)
		}
		out, err = ex.Execute(ctx, to("paid"))
		if err != nil || !out.Noop || out.Reason != CodeAlreadyInState {
			t.Fatalf("from == to: out=%+v err=%v", out, err)
		}
		_, err = ex.Execute(ctx, to("new"))
		if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != CodeInvalidTransition {
			t.Fatalf("invalid: %+v", ee)
		}
		if stageOf(t, svc, org, id) != "paid" {
			t.Fatal("a refused move must not write")
		}
		// the real Service gate is the one that decides, too: a stale read of
		// the row cannot slip a forbidden move past it
		_, err = svc.Update(ctx, "inbound_stage_orders", dynamic.NewSystemCaller(org), id, map[string]any{"status": "new"})
		if !errors.Is(err, dynamic.ErrInvalidTransition) {
			t.Fatalf("service gate: %v", err)
		}
	})
}

func TestDynamicRecordRestore(t *testing.T) {
	eachStageDB(t, func(t *testing.T, db *gorm.DB) {
		ex, svc := newStageExec(t, db)
		ctx := context.Background()
		org, other := uuid.New(), uuid.New()
		id := seedStageOrder(t, svc, org, "R1", "new")
		if err := svc.Delete(ctx, "inbound_stage_orders", dynamic.NewSystemCaller(org), id); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Get(ctx, "inbound_stage_orders", dynamic.NewSystemCaller(org), id); err == nil {
			t.Fatal("the row must be hidden after the delete")
		}

		// another organization cannot see the tombstone
		out, err := ex.Execute(ctx, stageOrderOp(other, OpRestore, "R1"))
		if err != nil || !out.NotFound || out.Affected != 0 {
			t.Fatalf("other org: out=%+v err=%v", out, err)
		}
		out, err = ex.Execute(ctx, stageOrderOp(org, OpRestore, "R1"))
		if err != nil || out.Affected != 1 || out.RecordID != id.String() || out.NotFound {
			t.Fatalf("restore: out=%+v err=%v", out, err)
		}
		if stageOf(t, svc, org, id) != "new" {
			t.Fatal("the row is not live again")
		}
		// nothing deleted matches any more: the row is live (a retry converges)
		out, err = ex.Execute(ctx, stageOrderOp(org, OpRestore, "R1"))
		if err != nil || !out.Noop || out.Reason != CodeAlreadyLive {
			t.Fatalf("again: out=%+v err=%v", out, err)
		}
		out, err = ex.Execute(ctx, stageOrderOp(org, OpRestore, "NOPE"))
		if err != nil || !out.NotFound {
			t.Fatalf("no match: out=%+v err=%v", out, err)
		}
	})
}

func TestDynamicExecutorCheckContractStateOps(t *testing.T) {
	eachStageDB(t, func(t *testing.T, db *gorm.DB) {
		ex, _ := newStageExec(t, db)
		ctx := context.Background()
		mk := func(a Action) Contract {
			a.Model, a.Match = "inbound_stage_orders", []string{"reference"}
			a.Mapping = map[string]string{"reference": "{{payload.reference}}"}
			return Contract{Key: "order.state", Version: 1, Actions: []Action{a}}
		}
		for name, a := range map[string]Action{
			"action":     {Do: ActionRecordAction, ActionKey: "mark_paid"},
			"transition": {Do: ActionRecordTransition, To: "shipped"},
			"restore":    {Do: ActionRecordRestore},
		} {
			if err := ex.CheckContract(ctx, mk(a)); err != nil {
				t.Errorf("%s must register: %v", name, err)
			}
		}
		for name, a := range map[string]Action{
			"closed action":   {Do: ActionRecordAction, ActionKey: "capture"},
			"approval action": {Do: ActionRecordAction, ActionKey: "approve"},
			"unknown action":  {Do: ActionRecordAction, ActionKey: "ghost"},
			"unknown state":   {Do: ActionRecordTransition, To: "lost"},
		} {
			if err := ex.CheckContract(ctx, mk(a)); !errors.Is(err, ErrInvalidContract) {
				t.Errorf("%s must be rejected: %v", name, err)
			}
		}
	})
}
