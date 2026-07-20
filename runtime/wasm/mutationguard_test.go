package wasm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// TestExecuteDataMutate_GuardViolationRollsBack — a mutation guard error on a
// create rolls the transaction back, surfaces constraint_violation, and emits
// NO canonical event.
func TestExecuteDataMutate_GuardViolationRollsBack(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.created")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`INSERT INTO "stock" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowID, orgID.String(), int64(-3)))
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	var guardTable string
	var guardRow map[string]any
	inv.mutationGuard = func(_ context.Context, table string, row map[string]any) error {
		guardTable = table
		guardRow = row
		return errors.New("constraint quantity_non_negative violated: quantity >= 0")
	}

	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"id": "`+rowID+`",
		"data": {"quantity": -3}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success {
		t.Fatalf("expected failure, got %s", out)
	}
	if env.Error == nil || env.Error.Code != "constraint_violation" {
		t.Fatalf("expected constraint_violation, got %#v", env.Error)
	}
	if guardTable != "stock" {
		t.Fatalf("guard must receive the LOGICAL table, got %q", guardTable)
	}
	if guardRow == nil || guardRow["quantity"] != float64(-3) && guardRow["quantity"] != int64(-3) {
		t.Fatalf("guard must receive the post-mutation row, got %#v", guardRow)
	}
	if evs := getEvents(); len(evs) != 0 {
		t.Fatalf("no canonical event may be published on a guard rollback, got %d", len(evs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestExecuteDataMutate_GuardSkippedOnDelete — deletes carry no resulting row
// state, so the guard must not run.
func TestExecuteDataMutate_GuardSkippedOnDelete(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.deleted")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowID, orgID.String(), int64(2)))
	mock.ExpectExec(`DELETE FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	inv.mutationGuard = func(_ context.Context, table string, _ map[string]any) error {
		t.Fatalf("guard must not run on delete (table %q)", table)
		return nil
	}

	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "delete", "table": "stock", "model": "Stock",
		"id": "`+rowID+`"
	}`))

	env := unmarshalMutate(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestExecuteDataBatch_GuardViolationRollsBackWholeBatch — a guard failure on
// the SECOND mutation rolls back the entire batch (including the already
// applied first row) and publishes nothing.
func TestExecuteDataBatch_GuardViolationRollsBackWholeBatch(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowA := uuid.NewString()
	rowB := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.*")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`INSERT INTO "stock" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowA, orgID.String(), int64(4)))
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`INSERT INTO "stock" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(rowB, orgID.String(), int64(-1)))
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	calls := 0
	inv.mutationGuard = func(_ context.Context, _ string, row map[string]any) error {
		calls++
		if q, ok := row["quantity"].(int64); ok && q < 0 {
			return errors.New("quantity >= 0")
		}
		if q, ok := row["quantity"].(float64); ok && q < 0 {
			return errors.New("quantity >= 0")
		}
		return nil
	}

	out := executeDataBatch(context.Background(), inv, []byte(`{
		"mutations": [
			{"op":"create","table":"stock","model":"Stock","id":"`+rowA+`","data":{"quantity":4}},
			{"op":"create","table":"stock","model":"Stock","id":"`+rowB+`","data":{"quantity":-1}}
		]
	}`))

	env := unmarshalMutate(t, out)
	if env.Success {
		t.Fatalf("expected failure, got %s", out)
	}
	if env.Error == nil || env.Error.Code != "constraint_violation" {
		t.Fatalf("expected constraint_violation, got %#v", env.Error)
	}
	if want := "mutations[1]"; env.Error != nil && !strings.Contains(env.Error.Message, want) {
		t.Fatalf("error must name the offending index %q, got %q", want, env.Error.Message)
	}
	if calls != 2 {
		t.Fatalf("guard must run once per applied row before the failure, got %d", calls)
	}
	if evs := getEvents(); len(evs) != 0 {
		t.Fatalf("no canonical events may be published on a guard rollback, got %d", len(evs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}
