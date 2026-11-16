package wasm

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/runtime/entityscope"
)

// The data_query wall (RFC 0001 §5, "Lectura WASM con contexto de A no devuelve
// filas de B"). sqlmock pins the exact predicate the host emits; the semantics
// of `branch_id IN (...)` are Postgres'. What these tests guarantee is that the
// host ALWAYS emits it in enforce mode, in the right position, bound to the
// scope's branches only, and never in the modes / tables where it must not.

const (
	wallBranch1 = "11111111-1111-1111-1111-111111111111"
	wallBranch2 = "22222222-2222-2222-2222-222222222222"
	wallForeign = "99999999-9999-9999-9999-999999999999"
)

func wallScope() dynamic.EntityScope {
	return dynamic.EntityScope{EntityID: "A", BranchIDs: []string{wallBranch2, wallBranch1}, Restricted: true}
}

func runWallQuery(t *testing.T, mode entityscope.Mode, scope *dynamic.EntityScope, probeCols []string, sqlRe string, args []driverArg, rows *sqlmock.Rows, reqJSON string) dqrEnvelope {
	t.Helper()
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()

	expectProbe(mock, `"stock"`, probeCols...)
	exp := mock.ExpectQuery(sqlRe)
	vals := make([]driverArg, 0, len(args)+1)
	vals = append(vals, orgID)
	vals = append(vals, args...)
	exp.WithArgs(vals...).WillReturnRows(rows)

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	inv.entityMode = mode
	ctx := context.Background()
	if scope != nil {
		ctx = dynamic.WithEntityScope(ctx, *scope)
	}
	out := executeDataQueryRecords(ctx, inv, []byte(reqJSON))
	env := unmarshalDataQuery(t, out)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v\nresponse: %s", err, out)
	}
	return env
}

// driverArg is the value type sqlmock matches arguments and rows with.
type driverArg = driver.Value

func rowsStock(vals ...[]driver.Value) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"id", "branch_id", "status"})
	for _, v := range vals {
		r.AddRow(v...)
	}
	return r
}

var withBranchCols = []string{"id", "organization_id", "branch_id", "status"}

func TestDataQueryWall_EnforceNarrowsToTheEntitysBranches(t *testing.T) {
	scope := wallScope()
	env := runWallQuery(t, entityscope.Enforce, &scope, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 AND branch_id IN \(\$2, \$3\) AND "status" = \$4 LIMIT 50`,
		[]driverArg{wallBranch1, wallBranch2, "active"},
		rowsStock([]driver.Value{"r1", wallBranch1, "active"}),
		`{"table": "stock", "where": {"status": "active"}}`)
	if !env.Success || env.Data == nil || len(env.Data.Rows) != 1 {
		t.Fatalf("expected the entity's own row, got %+v", env)
	}
}

func TestDataQueryWall_EntityPredicateSitsBetweenOrgScopeAndGuestFilters(t *testing.T) {
	// Guest filter numbering must continue AFTER the entity placeholders: a guest
	// that supplies its own branch_id filter cannot widen the wall, it can only
	// narrow it further (the two are ANDed).
	scope := wallScope()
	env := runWallQuery(t, entityscope.Enforce, &scope, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 AND branch_id IN \(\$2, \$3\) AND "branch_id" = \$4 LIMIT 50`,
		[]driverArg{wallBranch1, wallBranch2, wallForeign},
		rowsStock(),
		`{"table": "stock", "where": {"branch_id": "`+wallForeign+`"}}`)
	if !env.Success || env.Data == nil || len(env.Data.Rows) != 0 {
		t.Fatalf("asking for another entity's branch by filter must return nothing, got %+v", env)
	}
}

func TestDataQueryWall_IncludeUnassignedForTheDefaultEntity(t *testing.T) {
	scope := wallScope()
	scope.IncludeUnassigned = true
	runWallQuery(t, entityscope.Enforce, &scope, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 AND \(branch_id IN \(\$2, \$3\) OR branch_id IS NULL\) LIMIT 50`,
		[]driverArg{wallBranch1, wallBranch2},
		rowsStock(),
		`{"table": "stock"}`)
}

func TestDataQueryWall_UnresolvedScopeFailsClosed(t *testing.T) {
	scope := dynamic.EntityScope{EntityID: "A", Unresolved: true}
	env := runWallQuery(t, entityscope.Enforce, &scope, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 AND 1 = 0 LIMIT 50`,
		nil, rowsStock(), `{"table": "stock"}`)
	if !env.Success || len(env.Data.Rows) != 0 {
		t.Fatalf("an unresolved scope must read nothing, got %+v", env)
	}
}

func TestDataQueryWall_TableWithoutBranchIsScopedByOrganizationAlone(t *testing.T) {
	scope := wallScope()
	runWallQuery(t, entityscope.Enforce, &scope, []string{"id", "organization_id", "status"},
		`SELECT \* FROM "stock" WHERE organization_id = \$1 AND "status" = \$2 LIMIT 50`,
		[]driverArg{"active"},
		sqlmock.NewRows([]string{"id", "status"}).AddRow("r1", "active"),
		`{"table": "stock", "where": {"status": "active"}}`)
}

func TestDataQueryWall_OffAndMissingScopeLeaveTheQueryExactlyAsBefore(t *testing.T) {
	scope := wallScope()
	// mode off, scope present
	runWallQuery(t, entityscope.Off, &scope, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 LIMIT 50`,
		nil, rowsStock(), `{"table": "stock"}`)
	// enforce, but the call carried neither entity nor branch (no scope on ctx)
	runWallQuery(t, entityscope.Enforce, nil, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 LIMIT 50`,
		nil, rowsStock(), `{"table": "stock"}`)
	// zero-value (unrestricted) scope in enforce mode
	open := dynamic.EntityScope{}
	runWallQuery(t, entityscope.Enforce, &open, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 LIMIT 50`,
		nil, rowsStock(), `{"table": "stock"}`)
}

func TestDataQueryWall_LogModeObservesButNeverFilters(t *testing.T) {
	scope := wallScope()
	env := runWallQuery(t, entityscope.Log, &scope, withBranchCols,
		`SELECT \* FROM "stock" WHERE organization_id = \$1 LIMIT 50`,
		nil,
		rowsStock([]driver.Value{"r1", wallBranch1, "active"}, []driver.Value{"r2", wallForeign, "active"}),
		`{"table": "stock"}`)
	if !env.Success || len(env.Data.Rows) != 2 {
		t.Fatalf("log mode must return exactly what it returned before, got %+v", env)
	}
	// What it WOULD have hidden is what the log line reports.
	if got := entityscope.OutOfScope(env.Data.Rows, scope); got != 1 {
		t.Fatalf("expected 1 out-of-scope row to be reported, got %d", got)
	}
}
