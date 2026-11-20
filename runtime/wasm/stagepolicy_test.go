package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// K3b: data_mutate / data_batch hold a guest to the same per-transition policy
// as Service.Update. The mutation runs on sqlmock; the approvals live in a real
// dynamic.Service over sqlite so the parked request is checked for real.
//
//	draft -> posted (roles manager; requires_fields ref)
//	posted -> void  (approval manager)   posted: locked, editable_when_locked [note]
func policyStageMachine() StageMachineFn {
	return func(_, model string) *dynamic.StageMachine {
		if model != "Order" {
			return nil
		}
		return &dynamic.StageMachine{
			Field: "status",
			Stages: []manifest.StageDef{
				{Key: "draft"},
				{Key: "posted", Locked: true, EditableWhenLocked: []string{"note"}},
				{Key: "void", IsFinal: true, Locked: true},
			},
			Transitions: []manifest.TransitionDef{
				{From: "draft", To: "posted", Roles: []string{"manager"}, RequiresFields: []string{"ref"}},
				{From: "posted", To: "void", Approval: &manifest.ApprovalDef{Roles: []string{"manager"}, Label: "Cancelar venta"}},
			},
		}
	}
}

func policySvc(t *testing.T, mode dynamic.WasmStagePolicyMode, roles map[string][]string) *dynamic.Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return dynamic.New(dynamic.Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		WasmStagePolicy:              mode,
		ActorRolesResolver: func(_ context.Context, u modelbase.AuthUser) []string {
			return roles[u.GetID().String()]
		},
	})
}

func policyInv(t *testing.T, svc *dynamic.Service, gdb *gorm.DB, orgID uuid.UUID) *invocation {
	bus, _, _ := captureBus(t, "inventory.*")
	inv := testInvocation(gdb, bus, orgID, multiWriteEnforcer("orders"), nil)
	inv.stageMachine = policyStageMachine()
	inv.stagePolicy = svc.EnforceWasmStageWrite
	return inv
}

func expectRow(mock sqlmock.Sqlmock, orgID uuid.UUID, rowID, status string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "orders" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "status"}))
	mock.ExpectQuery(`SELECT \* FROM "orders" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "ref", "note"}).AddRow(rowID, status, "R-1", "n"))
}

func rawEnv(t *testing.T, out []byte) (bool, map[string]any) {
	t.Helper()
	var env struct {
		Success bool           `json:"success"`
		Error   map[string]any `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("%v -- %s", err, out)
	}
	return env.Success, env.Error
}

func mutateJSON(op, id, data string) []byte {
	return []byte(fmt.Sprintf(`{"op":%q,"table":"orders","model":"Order","id":%q,"data":%s}`, op, id, data))
}

func TestStagePolicy_Enforce_RolesRefusedBeforeUpdate(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org, row, agent := uuid.New(), uuid.NewString(), uuid.NewString()
	svc := policySvc(t, dynamic.WasmStagePolicyEnforce, map[string][]string{agent: {"agent"}})
	inv := policyInv(t, svc, gdb, org)
	expectRow(mock, org, row, "draft")
	mock.ExpectRollback() // NO UPDATE expected: a refused move never reaches it

	ctx := dynamic.WithActorID(context.Background(), agent)
	ok, e := rawEnv(t, executeDataMutate(ctx, inv, mutateJSON("update", row, `{"status":"posted","ref":"X"}`)))
	if ok || e["code"] != "forbidden" {
		t.Fatalf("agent posting: %v", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStagePolicy_Enforce_RequiresFieldsAndSystemActor(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org, row, mgr := uuid.New(), uuid.NewString(), uuid.NewString()
	svc := policySvc(t, dynamic.WasmStagePolicyEnforce, map[string][]string{mgr: {"manager"}})
	inv := policyInv(t, svc, gdb, org)

	// requires_fields: ref empty on the merged row.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "orders" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "orders" WHERE id = \$1 AND organization_id = \$2`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "ref"}).AddRow(row, "draft", ""))
	mock.ExpectRollback()
	ctx := dynamic.WithActorID(context.Background(), mgr)
	ok, e := rawEnv(t, executeDataMutate(ctx, inv, mutateJSON("update", row, `{"status":"posted"}`)))
	if ok || e["code"] != "validation_failed" {
		t.Fatalf("missing ref: %v", e)
	}

	// system principal (no actor) cannot skip roles either.
	expectRow(mock, org, row, "draft")
	mock.ExpectRollback()
	ok, e = rawEnv(t, executeDataMutate(context.Background(), inv, mutateJSON("update", row, `{"status":"posted","ref":"X"}`)))
	if ok || e["code"] != "forbidden" {
		t.Fatalf("system posting: %v", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStagePolicy_Enforce_LockedStage(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org, row, mgr := uuid.New(), uuid.NewString(), uuid.NewString()
	svc := policySvc(t, dynamic.WasmStagePolicyEnforce, map[string][]string{mgr: {"manager"}})
	inv := policyInv(t, svc, gdb, org)
	ctx := dynamic.WithActorID(context.Background(), mgr)

	expectRow(mock, org, row, "posted")
	mock.ExpectRollback()
	ok, e := rawEnv(t, executeDataMutate(ctx, inv, mutateJSON("update", row, `{"total":99}`)))
	if ok || e["code"] != "record_locked" {
		t.Fatalf("edit locked: %v", e)
	}

	// delete of a locked row.
	expectRow(mock, org, row, "posted")
	mock.ExpectRollback()
	ok, e = rawEnv(t, executeDataMutate(ctx, inv, []byte(fmt.Sprintf(`{"op":"delete","table":"orders","model":"Order","id":%q}`, row))))
	if ok || e["code"] != "record_locked" {
		t.Fatalf("delete locked: %v", e)
	}

	// editable_when_locked column goes through.
	expectRow(mock, org, row, "posted")
	mock.ExpectQuery(`UPDATE "orders" SET "note" = \$1, "updated_at" = \$2 WHERE id = \$3 AND organization_id = \$4 RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "note"}).AddRow(row, "posted", "m"))
	mock.ExpectCommit()
	if ok, e = rawEnv(t, executeDataMutate(ctx, inv, mutateJSON("update", row, `{"note":"m"}`))); !ok {
		t.Fatalf("editable column: %v", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Cancelling a posted sale from wasm parks the approval (same request and key as
// the REST path) and answers approval_required with policy + context + id.
func TestStagePolicy_Enforce_ApprovalParkedViaWasm(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org, row, seller := uuid.New(), uuid.NewString(), uuid.NewString()
	svc := policySvc(t, dynamic.WasmStagePolicyEnforce, map[string][]string{seller: {"cashier"}})
	inv := policyInv(t, svc, gdb, org)
	ctx := dynamic.WithActorID(context.Background(), seller)

	expectRow(mock, org, row, "posted")
	mock.ExpectRollback()
	out := executeDataMutate(ctx, inv, mutateJSON("update", row, `{"status":"void"}`))
	ok, e := rawEnv(t, out)
	if ok || e["code"] != "approval_required" {
		t.Fatalf("cancel: %s", out)
	}
	pol, _ := e["policy"].(map[string]any)
	if pol["key"] != "transition:posted->void" || pol["label"] != "Cancelar venta" {
		t.Fatalf("policy = %v", pol)
	}
	if c, _ := e["context"].(map[string]any); c["expr"] != "posted -> void" {
		t.Fatalf("context = %v", e["context"])
	}
	id, _ := e["approval_request_id"].(string)
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("approval_request_id = %v", e["approval_request_id"])
	}
	approver := dynamicApprover(t, svc, org, "manager")
	req, err := svc.GetApproval(context.Background(), approver, uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != dynamic.ApprovalStatusPending || req.ConstraintKey != "transition:posted->void" || req.RequestedBy.String() != seller {
		t.Fatalf("parked request = %+v", req)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// data_batch: the parked move aborts the WHOLE batch (nothing half-applied).
func TestStagePolicy_Enforce_BatchRollsBackOnRefusal(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org, a, b := uuid.New(), uuid.NewString(), uuid.NewString()
	svc := policySvc(t, dynamic.WasmStagePolicyEnforce, nil)
	inv := policyInv(t, svc, gdb, org)
	inv.enforcer = multiWriteEnforcer("orders")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "orders" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "orders" WHERE id = \$1 AND organization_id = \$2`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "note"}).AddRow(a, "posted", "n"))
	mock.ExpectQuery(`UPDATE "orders" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "note"}).AddRow(a, "posted", "x"))
	mock.ExpectQuery(`SELECT \* FROM "orders" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "orders" WHERE id = \$1 AND organization_id = \$2`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(b, "posted"))
	mock.ExpectRollback()

	out := executeDataBatch(context.Background(), inv, []byte(fmt.Sprintf(`{"mutations":[
		{"op":"update","table":"orders","model":"Order","id":%q,"data":{"note":"x"}},
		{"op":"update","table":"orders","model":"Order","id":%q,"data":{"status":"void"}}
	]}`, a, b)))
	ok, e := rawEnv(t, out)
	if ok || e["code"] != "approval_required" || !strings.Contains(fmt.Sprint(e["message"]), "approval required") {
		t.Fatalf("batch: %s", out)
	}
	if _, has := e["approval_request_id"]; !has {
		t.Fatalf("batch envelope lacks approval_request_id: %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// DEFAULT mode (warn): the legacy write still goes through, and the bypass is
// logged so addons can be fixed before enforce becomes the default.
func TestStagePolicy_WarnModeDoesNotBlockButLogs(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org, row := uuid.New(), uuid.NewString()
	svc := policySvc(t, "", nil) // default
	if svc.WasmStagePolicy() != dynamic.WasmStagePolicyWarn {
		t.Fatalf("default = %q", svc.WasmStagePolicy())
	}
	inv := policyInv(t, svc, gdb, org)
	expectRow(mock, org, row, "posted")
	mock.ExpectQuery(`UPDATE "orders" SET "status" = \$1, "updated_at" = \$2 WHERE id = \$3 AND organization_id = \$4 RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(row, "void"))
	mock.ExpectCommit()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	out := executeDataMutate(context.Background(), inv, mutateJSON("update", row, `{"status":"void"}`))
	if ok, e := rawEnv(t, out); !ok {
		t.Fatalf("warn must not block: %v", e)
	}
	if !strings.Contains(buf.String(), "stage_policy.wasm_bypass") || !strings.Contains(buf.String(), "posted->void") {
		t.Fatalf("missing wasm_bypass log: %q", buf.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A host that never calls WithStagePolicy keeps the legacy behaviour.
func TestStagePolicy_UnwiredHostKeepsLegacyWhitelist(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org, row := uuid.New(), uuid.NewString()
	inv := policyInv(t, policySvc(t, dynamic.WasmStagePolicyEnforce, nil), gdb, org)
	inv.stagePolicy = nil
	expectRow(mock, org, row, "posted")
	mock.ExpectQuery(`UPDATE "orders" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(row, "void"))
	mock.ExpectCommit()
	if ok, e := rawEnv(t, executeDataMutate(context.Background(), inv, mutateJSON("update", row, `{"status":"void"}`))); !ok {
		t.Fatalf("legacy: %v", e)
	}
}

func dynamicApprover(t *testing.T, svc *dynamic.Service, org uuid.UUID, role string) modelbase.AuthUser {
	t.Helper()
	return dynamic.NewSystemCaller(org)
}
