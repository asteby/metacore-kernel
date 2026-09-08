package wasm

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TestExecuteDataMutate_ComputeRunsOnUpdate is the ops#1403 regression at the
// wasm tier: a guest UPDATE must hand the post-mutation row to the compute pass
// INSIDE the open transaction, so a rollup declared over the mutated table is
// maintained instead of drifting. Before this hook the guest write path was a
// blind spot for the compute engine and every parent aggregate went stale.
func TestExecuteDataMutate_ComputeRunsOnUpdate(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.updated")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowID, orgID.String(), int64(5)))
	mock.ExpectQuery(`UPDATE "stock" SET .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowID, orgID.String(), int64(12)))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	var gotTable, gotAction string
	var gotRow map[string]any
	var gotTx *gorm.DB
	inv.mutationCompute = func(_ context.Context, tx *gorm.DB, table, action string, row map[string]any) error {
		gotTx, gotTable, gotAction, gotRow = tx, table, action, row
		return nil
	}

	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "update", "table": "stock", "model": "Stock",
		"id": "`+rowID+`",
		"data": {"quantity": 12}
	}`))

	env := unmarshalMutate(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if gotTable != "stock" {
		t.Fatalf("compute must receive the LOGICAL table, got %q", gotTable)
	}
	if gotAction != "updated" {
		t.Fatalf("compute action = %q, want \"updated\"", gotAction)
	}
	if gotRow == nil || (gotRow["quantity"] != float64(12) && gotRow["quantity"] != int64(12)) {
		t.Fatalf("compute must receive the post-mutation row, got %#v", gotRow)
	}
	if gotTx == nil {
		t.Fatal("compute must receive the open transaction, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestExecuteDataMutate_ComputeRunsOnDeleteWithBeforeRow — unlike the guard,
// the compute pass DOES run on a delete (an aggregate must fall when a child
// disappears) and receives the PRE-delete row, the only place the parent FK
// still exists.
func TestExecuteDataMutate_ComputeRunsOnDeleteWithBeforeRow(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.deleted")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowID, orgID.String(), int64(2)))
	mock.ExpectExec(`DELETE FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	var gotAction string
	var gotRow map[string]any
	inv.mutationCompute = func(_ context.Context, _ *gorm.DB, _, action string, row map[string]any) error {
		gotAction, gotRow = action, row
		return nil
	}

	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "delete", "table": "stock", "model": "Stock",
		"id": "`+rowID+`"
	}`))

	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if gotAction != "deleted" {
		t.Fatalf("compute action = %q, want \"deleted\"", gotAction)
	}
	if gotRow == nil || (gotRow["quantity"] != float64(2) && gotRow["quantity"] != int64(2)) {
		t.Fatalf("compute must receive the PRE-delete row, got %#v", gotRow)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestExecuteDataMutate_ComputeFailureRollsBack — a compute error must roll the
// child write back rather than commit a mutation whose aggregates could not be
// maintained.
func TestExecuteDataMutate_ComputeFailureRollsBack(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.updated")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowID, orgID.String(), int64(5)))
	mock.ExpectQuery(`UPDATE "stock" SET .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowID, orgID.String(), int64(12)))
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	inv.mutationCompute = func(_ context.Context, _ *gorm.DB, _, _ string, _ map[string]any) error {
		return errors.New("rollup target column missing")
	}

	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "update", "table": "stock", "model": "Stock",
		"id": "`+rowID+`",
		"data": {"quantity": 12}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success {
		t.Fatalf("expected failure, got %s", out)
	}
	if env.Error == nil || env.Error.Code != "db_error" {
		t.Fatalf("expected db_error, got %#v", env.Error)
	}
	if evs := getEvents(); len(evs) != 0 {
		t.Fatalf("no canonical event may be published on a compute rollback, got %d", len(evs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}
