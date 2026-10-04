package wasm

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
)

// Audit-column standard (docs/audit-columns.md): data_mutate records WHO on
// every write. The actor is the invocation's user; unattended work (schedule,
// webhook, connector — no actor in ctx) records dynamic.SystemActorID.

func TestDataMutateAudit_CreateStampsActorOrSystem(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx func() (context.Context, string)
	}{
		"user actor": {func() (context.Context, string) {
			a := uuid.NewString()
			return dynamic.WithActorID(context.Background(), a), a
		}},
		"no actor -> system actor": {func() (context.Context, string) {
			return context.Background(), dynamic.SystemActorID.String()
		}},
	} {
		t.Run(name, func(t *testing.T) {
			gdb, mock, cleanup := newMockGorm(t)
			defer cleanup()
			orgID, rowID := uuid.New(), uuid.NewString()
			ctx, want := tc.ctx()
			bus, _, _ := captureBus(t, "inventory.Stock.created")

			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).
				WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "created_at", "updated_at", "created_by_id", "updated_by_id", "quantity"}))
			mock.ExpectQuery(`INSERT INTO "stock" \("created_at", "created_by_id", "id", "organization_id", "quantity", "updated_at", "updated_by_id"\) VALUES \(\$1, \$2, \$3, \$4, \$5, \$6, \$7\) RETURNING \*`).
				WithArgs(sqlmock.AnyArg(), want, rowID, orgID, int64(5), sqlmock.AnyArg(), want).
				WillReturnRows(sqlmock.NewRows([]string{"id", "created_by_id", "updated_by_id"}).AddRow(rowID, want, want))
			mock.ExpectCommit()

			inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
			out := executeDataMutate(ctx, inv, []byte(`{"op":"create","table":"stock","model":"Stock","id":"`+rowID+`","data":{"quantity":5}}`))
			if env := unmarshalMutate(t, out); !env.Success {
				t.Fatalf("expected success, got %s", out)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations not met: %v", err)
			}
		})
	}
}

func TestDataMutateAudit_UpdateStampsUpdatedBy(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID, rowID, actor := uuid.New(), uuid.NewString(), uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.updated")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "updated_at", "updated_by_id"}))
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "note"}).AddRow(rowID, "old"))
	mock.ExpectQuery(`UPDATE "stock" SET "note" = \$1, "updated_at" = \$2, "updated_by_id" = \$3 WHERE id = \$4 AND organization_id = \$5 RETURNING \*`).
		WithArgs("new", sqlmock.AnyArg(), actor, rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "note"}).AddRow(rowID, "new"))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	ctx := dynamic.WithActorID(context.Background(), actor)
	out := executeDataMutate(ctx, inv, []byte(`{"op":"update","table":"stock","model":"Stock","id":"`+rowID+`","data":{"note":"new"}}`))
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestDataMutateAudit_SoftDeleteStampsDeletedBy(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID, rowID := uuid.New(), uuid.NewString()
	sys := dynamic.SystemActorID.String()
	bus, _, _ := captureBus(t, "inventory.Stock.deleted")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "deleted_at", "updated_by_id", "deleted_by_id"}))
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "deleted_at"}).AddRow(rowID, nil))
	// No actor in ctx (a schedule): the system actor is recorded.
	mock.ExpectExec(`UPDATE "stock" SET "deleted_at" = \$1, "updated_at" = \$2, "deleted_by_id" = \$3, "updated_by_id" = \$4 WHERE id = \$5 AND organization_id = \$6`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sys, sys, rowID, orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{"op":"delete","table":"stock","model":"Stock","id":"`+rowID+`"}`))
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// A ledger table (created_at, no updated_at) is not written with a phantom
// updated_at column.
func TestDataMutateAudit_LedgerCreateOmitsUpdatedAt(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID, rowID, actor := uuid.New(), uuid.NewString(), uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.created")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "created_at", "created_by_id", "quantity"}))
	mock.ExpectQuery(`INSERT INTO "stock" \("created_at", "created_by_id", "id", "organization_id", "quantity"\) VALUES \(\$1, \$2, \$3, \$4, \$5\) RETURNING \*`).
		WithArgs(sqlmock.AnyArg(), actor, rowID, orgID, int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(rowID))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	ctx := dynamic.WithActorID(context.Background(), actor)
	out := executeDataMutate(ctx, inv, []byte(`{"op":"create","table":"stock","model":"Stock","id":"`+rowID+`","data":{"quantity":1}}`))
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// A guest can never name the audit columns: the host stamps them.
func TestDataMutateAudit_GuestCannotSupplyAuditColumns(t *testing.T) {
	orgID := uuid.New()
	for _, col := range []string{"created_by_id", "updated_by_id", "deleted_by_id", "created_at", "deleted_at"} {
		gdb, mock, cleanup := newMockGorm(t)
		bus, _, _ := captureBus(t, "inventory.Stock.created")
		inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
		out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","data":{"`+col+`":"`+uuid.NewString()+`"}}`))
		if env := unmarshalMutate(t, out); env.Success {
			t.Errorf("%s: guest-supplied audit column must be rejected, got %s", col, out)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s: no DB work expected: %v", col, err)
		}
		cleanup()
	}
}
