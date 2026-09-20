package wasm

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
)

// A cross-record rule violation reported by the MutationComputeFn (what
// dynamic.CrossRecordCompute returns) must roll the guest's write back and reach
// the guest as the stable `constraint_violation` code — not `db_error`.
func TestExecuteDataMutate_CrossRecordViolationRollsBack(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.created")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`INSERT INTO "stock" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).AddRow(rowID, orgID.String(), int64(1)))
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	inv.mutationCompute = func(_ context.Context, _ *gorm.DB, _ uuid.UUID, _, _ string, _ map[string]any) error {
		return &dynamic.ConstraintError{ErrorKey: "pos.session_closed", Expr: "ref_state rx_sessions.state"}
	}

	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock", "id": "`+rowID+`", "data": {"quantity": 1}
	}`))
	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "constraint_violation" {
		t.Fatalf("expected constraint_violation, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "pos.session_closed") {
		t.Fatalf("message must carry the rule's error_key, got %q", env.Error.Message)
	}
	if evs := getEvents(); len(evs) != 0 {
		t.Fatalf("no canonical event on rollback, got %d", len(evs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataBatch_CrossRecordViolationRollsBackWholeBatch(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowA, rowB := uuid.NewString(), uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.*")

	mock.ExpectBegin()
	for _, id := range []string{rowA, rowB} {
		mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
		mock.ExpectQuery(`INSERT INTO "stock" .* RETURNING \*`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).AddRow(id, orgID.String(), int64(1)))
	}
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	calls := 0
	inv.mutationCompute = func(_ context.Context, _ *gorm.DB, _ uuid.UUID, _, _ string, _ map[string]any) error {
		calls++
		if calls == 2 {
			return &dynamic.ConstraintError{ErrorKey: "pos.overpay", Expr: "sum(amount) <= rx_orders.total"}
		}
		return nil
	}

	out := executeDataBatch(context.Background(), inv, []byte(`{"mutations": [
		{"op":"create","table":"stock","model":"Stock","id":"`+rowA+`","data":{"quantity":1}},
		{"op":"create","table":"stock","model":"Stock","id":"`+rowB+`","data":{"quantity":1}}
	]}`))
	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "constraint_violation" {
		t.Fatalf("expected constraint_violation, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "mutations[1]") {
		t.Fatalf("error must name the offending index, got %q", env.Error.Message)
	}
	if evs := getEvents(); len(evs) != 0 {
		t.Fatalf("no canonical events on rollback, got %d", len(evs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}
