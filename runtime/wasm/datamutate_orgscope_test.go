package wasm

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

func lineWriteEnforcer() *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{
			{Kind: "db:write", Target: "purchase_order_items"},
		})
	})
	e.SetMode(security.ModeEnforce)
	return e
}

// expectChildProbes declares the two introspection queries a create on an
// org-less table issues: the column probe (no organization_id) and the FK walk.
func expectChildProbes(mock sqlmock.Sqlmock, fkRowSet *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT \* FROM "purchase_order_items" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "purchase_order_id", "qty"}))
	mock.ExpectQuery(`FROM pg_constraint`).WillReturnRows(fkRowSet)
	mock.ExpectQuery(`SELECT \* FROM "public"\."purchase_orders" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
}

func poFKRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"fk_column", "parent_schema", "parent_table", "parent_column"}).
		AddRow("purchase_order_id", "public", "purchase_orders", "id")
}

// TestDataMutate_ChildCreateOmitsOrgAndVerifiesParent is the 42703 regression
// for the write side: the INSERT must NOT carry organization_id, and the
// parent link must be checked against the caller's org before it runs.
func TestDataMutate_ChildCreateOmitsOrgAndVerifiesParent(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "purchases.PurchaseOrderItem.created")

	mock.ExpectBegin()
	expectChildProbes(mock, poFKRows())
	// Tenancy check on the parent, scoped to the caller's org.
	mock.ExpectQuery(`SELECT 1 FROM "public"\."purchase_orders" WHERE "id" = \$1 AND organization_id = \$2 LIMIT 1`).
		WithArgs("po-1", orgID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	// No organization_id in the column list — this is the 42703 that was.
	mock.ExpectQuery(`INSERT INTO "purchase_order_items" \("created_at", "id", "purchase_order_id", "qty", "updated_at"\) VALUES \(\$1, \$2, \$3, \$4, \$5\) RETURNING \*`).
		WithArgs(sqlmock.AnyArg(), rowID, "po-1", int64(3), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "purchase_order_id"}).AddRow(rowID, "po-1"))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, lineWriteEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "purchase_order_items", "model": "PurchaseOrderItem",
		"id": "`+rowID+`",
		"data": {"purchase_order_id": "po-1", "qty": 3}
	}`))

	env := unmarshalMutate(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestDataMutate_ChildCreateRejectsForeignParent is the isolation guarantee:
// a guest passing a parent id from ANOTHER organization must be refused, not
// quietly allowed to hang a line off someone else's document. Omitting the
// column without this check is exactly the hole.
func TestDataMutate_ChildCreateRejectsForeignParent(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	callerOrg := uuid.New()
	bus, getEvents, _ := captureBus(t, "purchases.PurchaseOrderItem.created")

	mock.ExpectBegin()
	expectChildProbes(mock, poFKRows())
	// The parent belongs to another org → the scoped lookup finds nothing.
	mock.ExpectQuery(`SELECT 1 FROM "public"\."purchase_orders" WHERE "id" = \$1 AND organization_id = \$2 LIMIT 1`).
		WithArgs("po-de-otra-org", callerOrg).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	// No INSERT may follow.
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, callerOrg, lineWriteEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "purchase_order_items", "model": "PurchaseOrderItem",
		"data": {"purchase_order_id": "po-de-otra-org", "qty": 3}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success {
		t.Fatalf("cross-org child write was ACCEPTED: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if len(getEvents()) != 0 {
		t.Errorf("a rejected write published %d canonical events", len(getEvents()))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestDataMutate_ChildCreateRejectsMissingParentLink: the row supplies no
// verifiable parent link, so the host cannot attribute it to any organization.
// Inserting it would create a row that no org-scoped read can ever return.
func TestDataMutate_ChildCreateRejectsMissingParentLink(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	bus, _, _ := captureBus(t, "purchases.PurchaseOrderItem.created")

	mock.ExpectBegin()
	expectChildProbes(mock, poFKRows())
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, lineWriteEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "purchase_order_items", "model": "PurchaseOrderItem",
		"data": {"qty": 3}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success {
		t.Fatalf("unattributable row was written: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "parent links") {
		t.Errorf("error does not explain the refusal: %s", env.Error.Message)
	}
}

// TestDataMutate_ChildCreateRejectsUnscopableTable: org-less table with no
// qualifying FK at all → refuse rather than insert an orphan.
func TestDataMutate_ChildCreateRejectsUnscopableTable(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	bus, _, _ := captureBus(t, "purchases.PurchaseOrderItem.created")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "purchase_order_items" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "qty"}))
	mock.ExpectQuery(`FROM pg_constraint`).
		WillReturnRows(sqlmock.NewRows([]string{"fk_column", "parent_schema", "parent_table", "parent_column"}))
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, lineWriteEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "purchase_order_items", "model": "PurchaseOrderItem",
		"data": {"qty": 3}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success {
		t.Fatalf("unscopable table was written: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
}
