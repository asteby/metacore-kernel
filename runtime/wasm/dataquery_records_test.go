package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// dqrEnvelope is the data_query wire shape (docs/wasm-abi.md § 15.5).
type dqrEnvelope struct {
	Success bool           `json:"success"`
	Data    *dqrData       `json:"data,omitempty"`
	Error   *dbqError      `json:"error,omitempty"`
	Meta    map[string]any `json:"meta"`
}

type dqrData struct {
	Rows []map[string]any `json:"rows"`
}

func unmarshalDataQuery(t *testing.T, raw []byte) dqrEnvelope {
	t.Helper()
	var env dqrEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v -- %s", err, raw)
	}
	return env
}

// stockReadEnforcer grants `db:read stock` and nothing else — the shape a
// real manifest declaration compiles to. The implicit addon_<key>.* grant
// does NOT cover logical tables, mirroring data_mutate's write gate.
func stockReadEnforcer() *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{
			{Kind: "db:read", Target: "stock"},
		})
	})
	e.SetMode(security.ModeEnforce)
	return e
}

// expectProbe declares the zero-row column probe data_query issues before
// the real SELECT. The returned column set drives soft-delete detection.
func expectProbe(mock sqlmock.Sqlmock, tblRe string, columns ...string) {
	mock.ExpectQuery(`SELECT \* FROM ` + tblRe + ` LIMIT 0`).
		WillReturnRows(sqlmock.NewRows(columns))
}

func TestExecuteDataQueryRecords_WhereEqualityAndOrgScope(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()

	// No deleted_at in the probe → no soft-delete filter. The org predicate
	// is ALWAYS first and host-injected; guest filters follow sorted.
	expectProbe(mock, `"stock"`, "id", "organization_id", "product_id", "quantity", "status")
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE organization_id = \$1 AND "product_id" = \$2 AND "status" = \$3 LIMIT 50`).
		WithArgs(orgID, "prod-1", "active").
		WillReturnRows(sqlmock.NewRows([]string{"id", "product_id", "quantity"}).
			AddRow("r1", "prod-1", int64(7)).
			AddRow("r2", "prod-1", int64(3)))

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{
		"table": "stock",
		"where": {"status": "active", "product_id": "prod-1"}
	}`))

	env := unmarshalDataQuery(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data == nil || len(env.Data.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %#v", env.Data)
	}
	if env.Data.Rows[0]["quantity"] != float64(7) {
		t.Fatalf("expected quantity=7, got %#v", env.Data.Rows[0])
	}
	if env.Meta["orgId"] != orgID.String() {
		t.Fatalf("expected meta.orgId %s, got %v", orgID, env.Meta["orgId"])
	}
	if env.Meta["envelopeVersion"] != float64(1) {
		t.Fatalf("expected envelopeVersion 1, got %v", env.Meta["envelopeVersion"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataQueryRecords_DeletedAtFilterWhenColumnExists(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()

	// Probe sees deleted_at → the host appends `deleted_at IS NULL`.
	expectProbe(mock, `"stock"`, "id", "organization_id", "quantity", "deleted_at")
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE organization_id = \$1 AND deleted_at IS NULL LIMIT 50`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "quantity", "deleted_at"}).
			AddRow("r1", int64(7), nil))

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "stock"}`))

	env := unmarshalDataQuery(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if len(env.Data.Rows) != 1 {
		t.Fatalf("expected 1 row, got %#v", env.Data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataQueryRecords_LimitClampAndExplicit(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()

	// limit 999 clamps to the hard max 200.
	expectProbe(mock, `"stock"`, "id", "organization_id")
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE organization_id = \$1 LIMIT 200`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// limit 5 passes through untouched.
	expectProbe(mock, `"stock"`, "id", "organization_id")
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE organization_id = \$1 LIMIT 5`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	for _, req := range []string{
		`{"table": "stock", "limit": 999}`,
		`{"table": "stock", "limit": 5}`,
	} {
		out := executeDataQueryRecords(context.Background(), inv, []byte(req))
		if env := unmarshalDataQuery(t, out); !env.Success {
			t.Fatalf("expected success for %s, got %s", req, out)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataQueryRecords_TableResolverApplied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()

	// Capability checked on the LOGICAL name; SQL (probe + main) runs on
	// the resolver-produced physical name.
	expectProbe(mock, `"public"\."stock"`, "id", "organization_id")
	mock.ExpectQuery(`SELECT \* FROM "public"\."stock" WHERE organization_id = \$1 LIMIT 50`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("r1"))

	resolve := func(table string) string { return "public." + table }
	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), resolve)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "stock"}`))

	if env := unmarshalDataQuery(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataQueryRecords_CapabilityDenied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// The implicit addon_<key>.* grant does not cover logical tables — an
	// addon without a declared `db:read stock` is denied before any SQL.
	inv := testInvocation(gdb, nil, uuid.New(), permissiveEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "stock"}`))

	env := unmarshalDataQuery(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "db:read") {
		t.Fatalf("expected message to mention db:read, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDataQueryRecords_DbWriteImpliesRead(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	expectProbe(mock, `"stock"`, "id", "organization_id")
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE organization_id = \$1 LIMIT 50`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// A db:write grant satisfies db:read (security.CanReadModel semantics)
	// — the data_mutate consumer can read back what it writes without a
	// second declaration.
	inv := testInvocation(gdb, nil, orgID, stockWriteEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "stock"}`))

	if env := unmarshalDataQuery(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataQueryRecords_GuestOrgAndDeletedAtFiltersRejected(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	inv := testInvocation(gdb, nil, uuid.New(), stockReadEnforcer(), nil)
	for _, req := range []string{
		`{"table": "stock", "where": {"organization_id": "` + uuid.NewString() + `"}}`,
		`{"table": "stock", "where": {"deleted_at": null}}`,
	} {
		out := executeDataQueryRecords(context.Background(), inv, []byte(req))
		env := unmarshalDataQuery(t, out)
		if env.Success || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Errorf("expected invalid_request for %s, got %s", req, out)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDataQueryRecords_NullWhereCompilesToIsNull(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	expectProbe(mock, `"stock"`, "id", "organization_id", "note")
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE organization_id = \$1 AND "note" IS NULL LIMIT 50`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "note"}))

	inv := testInvocation(gdb, nil, orgID, stockReadEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{
		"table": "stock", "where": {"note": null}
	}`))

	if env := unmarshalDataQuery(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataQueryRecords_NoActiveOrg(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	inv := testInvocation(gdb, nil, uuid.Nil, stockReadEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table": "stock"}`))

	env := unmarshalDataQuery(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Fatalf("expected invalid_request for missing org, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDataQueryRecords_ValidateRequest(t *testing.T) {
	bad := []string{
		`{"table": "public.stock"}`,                       // qualified table
		`{"table": "stock; DROP"}`,                        // injection shape
		`{"table": ""}`,                                   // empty table
		`{"table": "stock", "limit": -1}`,                 // negative limit
		`{"table": "stock", "where": {"a;b": 1}}`,         // bad column
		`{"table": "stock", "where": {"meta": {"x": 1}}}`, // non-scalar value
		`{"table": "stock", "where": {"tags": [1, 2]}}`,   // non-scalar value
	}
	for _, req := range bad {
		inv := testInvocation(nil, nil, uuid.New(), nil, nil)
		out := executeDataQueryRecords(context.Background(), inv, []byte(req))
		env := unmarshalDataQuery(t, out)
		if env.Success || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Errorf("expected invalid_request for %s, got %s", req, out)
		}
	}
}
