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

// lineReadEnforcer grants `db:read purchase_order_items` — a CHILD table: its
// rows belong to an org only through their parent purchase order.
func lineReadEnforcer() *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{
			{Kind: "db:read", Target: "purchase_order_items"},
		})
	})
	e.SetMode(security.ModeEnforce)
	return e
}

// expectFKProbe declares the FK-introspection query the org-scope resolver
// issues for a table with no organization_id of its own.
func expectFKProbe(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`FROM pg_constraint`).WillReturnRows(rows)
}

func fkRows(fkCol, schema, table, parentCol string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"fk_column", "parent_schema", "parent_table", "parent_column"}).
		AddRow(fkCol, schema, table, parentCol)
}

// TestDataQuery_ChildTableScopesThroughParent is the 42703 regression: a child
// table with no organization_id must be readable — the bug that blocked
// purchases.receive_goods — but ONLY through an EXISTS join that pins the
// parent row to the caller's organization.
func TestDataQuery_ChildTableScopesThroughParent(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()

	// Child table: no organization_id in the probe (this used to 42703).
	expectProbe(mock, `"purchase_order_items"`, "id", "purchase_order_id", "qty")
	expectFKProbe(mock, fkRows("purchase_order_id", "public", "purchase_orders", "id"))
	// The parent is probed to confirm it actually carries organization_id.
	expectProbe(mock, `"public"\."purchase_orders"`, "id", "organization_id")

	mock.ExpectQuery(`SELECT \* FROM "purchase_order_items" WHERE EXISTS \(SELECT 1 FROM "public"\."purchase_orders" __org_scope_0 WHERE __org_scope_0\."id" = "purchase_order_items"\."purchase_order_id" AND __org_scope_0\.organization_id = \$1\) AND "purchase_order_id" = \$2 LIMIT 50`).
		WithArgs(orgID, "po-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "qty"}).AddRow("l1", int64(4)))

	inv := testInvocation(gdb, nil, orgID, lineReadEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{
		"table": "purchase_order_items",
		"where": {"purchase_order_id": "po-1"}
	}`))

	env := unmarshalDataQuery(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data == nil || len(env.Data.Rows) != 1 {
		t.Fatalf("expected 1 row, got %#v", env.Data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestDataQuery_ChildTableCannotBeReadCrossOrg is the isolation guarantee the
// fix has to carry: the tenant predicate must survive on a table with no
// organization_id. A guest asking for the child table with NO filters — the
// shape that would dump every org's rows if the scope were simply dropped —
// must still emit the parent EXISTS bound to $1 = the invocation's org.
func TestDataQuery_ChildTableCannotBeReadCrossOrg(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	callerOrg := uuid.New()

	expectProbe(mock, `"purchase_order_items"`, "id", "purchase_order_id")
	expectFKProbe(mock, fkRows("purchase_order_id", "public", "purchase_orders", "id"))
	expectProbe(mock, `"public"\."purchase_orders"`, "id", "organization_id")

	mock.ExpectQuery(`SELECT \* FROM "purchase_order_items" WHERE EXISTS \(.*organization_id = \$1\) LIMIT 50`).
		WithArgs(callerOrg).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("mine"))

	inv := testInvocation(gdb, nil, callerOrg, lineReadEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "purchase_order_items"}`))

	env := unmarshalDataQuery(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	// sqlmock's WithArgs(callerOrg) is the assertion: had the host dropped the
	// tenant predicate, the statement would carry no args and not match.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unscoped read of a child table: %v", err)
	}
}

// TestDataQuery_UnscopableTableIsRefused: no organization_id AND no NOT NULL FK
// to an org-scoped parent → the host cannot prove tenancy, so it must refuse
// instead of running an unscoped SELECT.
func TestDataQuery_UnscopableTableIsRefused(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()

	expectProbe(mock, `"purchase_order_items"`, "id", "note")
	expectFKProbe(mock, sqlmock.NewRows([]string{"fk_column", "parent_schema", "parent_table", "parent_column"}))

	inv := testInvocation(gdb, nil, orgID, lineReadEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "purchase_order_items"}`))

	env := unmarshalDataQuery(t, out)
	if env.Success {
		t.Fatalf("unscopable table was READ instead of refused: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected a forbidden error, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "no organization_id") {
		t.Errorf("error message does not explain the refusal: %s", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestDataQuery_FKToOrglessParentDoesNotQualify: a FK pointing at a table that
// is ITSELF org-less proves nothing about tenancy. It must not be accepted as
// a scope path — otherwise a chain of org-less tables would launder an
// unscoped read.
func TestDataQuery_FKToOrglessParentDoesNotQualify(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()

	expectProbe(mock, `"purchase_order_items"`, "id", "unit_id")
	expectFKProbe(mock, fkRows("unit_id", "public", "units", "id"))
	// The parent is a global catalog table: no organization_id.
	expectProbe(mock, `"public"\."units"`, "id", "label")

	inv := testInvocation(gdb, nil, orgID, lineReadEnforcer(), nil)
	inv.addonKey = "purchases"
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "purchase_order_items"}`))

	env := unmarshalDataQuery(t, out)
	if env.Success {
		t.Fatalf("read scoped through an org-less parent: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected a forbidden error, got %s", out)
	}
}

// TestOrgScopeClauses_TopLevelTableUnchanged pins the no-op case: a table with
// organization_id keeps the exact predicate it had before this change, and
// issues no FK introspection at all.
func TestOrgScopeClauses_TopLevelTableUnchanged(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	got, err := orgScopeClauses(gdb, `"stock"`, map[string]bool{"organization_id": true, "id": true}, "$1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "organization_id = $1" {
		t.Fatalf("got %#v, want the plain org predicate", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a top-level table should issue no extra queries: %v", err)
	}
}
