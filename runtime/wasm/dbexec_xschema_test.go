package wasm

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/manifest"
)

// -----------------------------------------------------------------------------
// extractMutationRelations — AST-level unit tests for the db_exec walker.
// -----------------------------------------------------------------------------

func TestExtractMutationRelations_InsertBareTarget(t *testing.T) {
	rels, err := extractMutationRelations("INSERT INTO tickets (title) VALUES ('x')")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "", Table: "tickets", Cap: capWrite}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_InsertSchemaQualifiedTarget(t *testing.T) {
	rels, err := extractMutationRelations("INSERT INTO public.users (id) VALUES (1)")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "public", Table: "users", Cap: capWrite}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_UpdateWithFromIsRead(t *testing.T) {
	// UPDATE … FROM source — target is capWrite, source is capRead.
	// The walker emits in stable order: refs sort by Cap then schema then
	// table, so db:read entries come first, db:write entries after.
	sql := `UPDATE addon_tickets.t SET status = s.value
			  FROM staging.s WHERE t.id = s.tid`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "staging", Table: "s", Cap: capRead},
		{Schema: "addon_tickets", Table: "t", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_DeleteUsingIsRead(t *testing.T) {
	sql := `DELETE FROM tickets USING public.users u WHERE tickets.uid = u.id`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "public", Table: "users", Cap: capRead},
		{Schema: "", Table: "tickets", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_MergeSourceIsRead(t *testing.T) {
	sql := `MERGE INTO tickets t
			  USING public.staging s ON t.id = s.id
			  WHEN MATCHED THEN UPDATE SET status = s.status`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "public", Table: "staging", Cap: capRead},
		{Schema: "", Table: "tickets", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_InsertSelectSourceIsRead(t *testing.T) {
	sql := `INSERT INTO addon_tickets.audit (note)
			SELECT comment FROM public.events WHERE kind = 'login'`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "public", Table: "events", Cap: capRead},
		{Schema: "addon_tickets", Table: "audit", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_DMLInsideCTEKeepsWriteCap(t *testing.T) {
	// Postgres allows DML inside a WITH clause (`WITH x AS (UPDATE …)`).
	// The inner UPDATE target must still emit capWrite, not capRead — a
	// previous walker that treated CTE bodies as read-only would have
	// missed this vector.
	sql := `WITH x AS (
				UPDATE public.users SET role = 'admin' RETURNING id
			)
			INSERT INTO tickets (uid) SELECT id FROM x`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "", Table: "x", Cap: capRead},
		{Schema: "", Table: "tickets", Cap: capWrite},
		{Schema: "public", Table: "users", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_ReturningSubqueryIsRead(t *testing.T) {
	sql := `INSERT INTO tickets (uid)
			VALUES (1)
			RETURNING (SELECT name FROM public.users WHERE id = 1)`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "public", Table: "users", Cap: capRead},
		{Schema: "", Table: "tickets", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// -----------------------------------------------------------------------------
// RangeFunction — function-as-table reaches the gate.
// -----------------------------------------------------------------------------

func TestExtractRelations_RangeFunctionBareName(t *testing.T) {
	rels, err := extractRelations("SELECT * FROM my_func()")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "", Table: "my_func", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_RangeFunctionSchemaQualified(t *testing.T) {
	rels, err := extractRelations("SELECT * FROM other_addon.my_func(1, 'x')")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "other_addon", Table: "my_func", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractMutationRelations_RangeFunctionGatesBothCaps(t *testing.T) {
	// A function-as-table reached from within a DML scope must trip both
	// gates — a `setof`-returning function can read and write, and the AST
	// gives us no way to tell which. The walker emits the read entry first
	// (capRead < capWrite), then the write target, then the write entry for
	// the function — sorted by (cap, schema, table).
	sql := `INSERT INTO tickets (id)
			SELECT id FROM other_addon.iterate()`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "other_addon", Table: "iterate", Cap: capRead},
		{Schema: "", Table: "tickets", Cap: capWrite},
		{Schema: "other_addon", Table: "iterate", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// -----------------------------------------------------------------------------
// executeDBExec — cross-schema gate integration tests with sqlmock. These
// mirror dbquery_xschema_test.go for the db_query path. All of these are
// regression tests for the v0.10.3 fix: pre-fix the host only checked the
// implicit `db:write addon_<key>.*` capability, so any cross-schema mutation
// reached the driver. With AST gating in place those must be denied unless
// an explicit `db:write <schema>.<rel>` (or `<schema>.*`) capability is
// declared.
// -----------------------------------------------------------------------------

// TestExecuteDBExec_BareTableAllowed asserts the v0.10.2 path is unchanged:
// a mutation against the addon's own implicit schema works without any
// extra capability declaration.
func TestExecuteDBExec_BareTableAllowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE tickets SET status`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := executeDBExec(context.Background(), gdb, nil, "tickets",
		enforcerWithCaps("tickets", nil),
		"UPDATE tickets SET status = 'closed' WHERE id = 1", nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBExec_OwnSchemaQualifiedAllowed — explicit own-schema target
// also passes without extra grants.
func TestExecuteDBExec_OwnSchemaQualifiedAllowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE addon_tickets.tickets SET status`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := executeDBExec(context.Background(), gdb, nil, "tickets",
		enforcerWithCaps("tickets", nil),
		"UPDATE addon_tickets.tickets SET status = 'closed'", nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBExec_UpdatePublicUsersWithoutCap_Denied is the regression for
// the v0.10.3 fix. Pre-fix this UPDATE reached the driver and could change
// the role column on a privileged user table; with AST gating in place it
// must be denied at the host before the driver is touched.
func TestExecuteDBExec_UpdatePublicUsersWithoutCap_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"UPDATE public.users SET role = 'admin'", nil)

	env := unmarshalExec(t, out)
	if env.Success {
		t.Fatalf("expected forbidden, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "public.users") {
		t.Errorf("expected message to mention public.users, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_UpdatePublicUsersWithStarCap_Allowed — same UPDATE, but
// the addon manifest declares `db:write public.*` so the relation passes.
func TestExecuteDBExec_UpdatePublicUsersWithStarCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE public.users SET role`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:write", Target: "public.*"}}
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", caps),
		"UPDATE public.users SET role = 'admin'", nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBExec_UpdatePublicUsersWithTableCap_Allowed — narrower grant
// (single relation) must also pass.
func TestExecuteDBExec_UpdatePublicUsersWithTableCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE public.users SET role`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:write", Target: "public.users"}}
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", caps),
		"UPDATE public.users SET role = 'admin'", nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBExec_InsertCrossSchemaWithoutCap_Denied — INSERT path is
// gated the same way as UPDATE.
func TestExecuteDBExec_InsertCrossSchemaWithoutCap_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"INSERT INTO public.audit (note) VALUES ('hi')", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "public.audit") {
		t.Errorf("expected message to mention public.audit, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_DeleteCrossSchemaWithoutCap_Denied — DELETE path gate.
func TestExecuteDBExec_DeleteCrossSchemaWithoutCap_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"DELETE FROM billing.invoices WHERE id = 1", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "billing.invoices") {
		t.Errorf("expected message to mention billing.invoices, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_MergeCrossSchemaWithoutCap_Denied — MERGE target gate.
func TestExecuteDBExec_MergeCrossSchemaWithoutCap_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `MERGE INTO billing.invoices i
			  USING staging s ON i.id = s.id
			  WHEN MATCHED THEN UPDATE SET amount = s.amount`
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "billing.invoices") {
		t.Errorf("expected message to mention billing.invoices, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_UpdateFromCrossSchemaSource_Denied — UPDATE target is the
// addon's own table but the FROM clause pulls from another schema. The
// source side needs `db:read`, not `db:write`, and missing read still denies.
func TestExecuteDBExec_UpdateFromCrossSchemaSource_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `UPDATE addon_tickets.tickets t
			  SET status = s.status
			  FROM staging.s WHERE t.id = s.tid`
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "staging.s") {
		t.Errorf("expected message to mention staging.s, got %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "db:read") {
		t.Errorf("expected message to mention db:read (source side), got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_UpdateFromCrossSchemaSourceWithReadCap_Allowed — same
// shape but with the read capability declared.
func TestExecuteDBExec_UpdateFromCrossSchemaSourceWithReadCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE addon_tickets.tickets`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:read", Target: "staging.s"}}
	sql := `UPDATE addon_tickets.tickets t
			  SET status = s.status
			  FROM staging.s WHERE t.id = s.tid`
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", caps), sql, nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBExec_CTEDMLCrossSchema_Denied — `WITH x AS (UPDATE other.t …)`
// hides the cross-schema mutation inside a CTE body. Pre-fix this slipped
// past the gate because the host only inspected the top-level statement.
func TestExecuteDBExec_CTEDMLCrossSchema_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `WITH x AS (
				UPDATE public.users SET role = 'admin' RETURNING id
			)
			INSERT INTO tickets (uid) SELECT id FROM x`
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "public.users") {
		t.Errorf("expected message to mention public.users, got %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "db:write") {
		t.Errorf("expected message to mention db:write, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_RejectsWithSelect — `WITH … SELECT` slips past the
// string-level leading-word check (we allow leading WITH so DML can use
// CTEs), but the AST top-level check inside extractMutationRelations must
// still refuse a pure SELECT top-level so db_exec can't be used as a
// SELECT bypass.
func TestExecuteDBExec_RejectsWithSelect(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"WITH x AS (SELECT 1) SELECT * FROM x", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql for WITH … SELECT top-level, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_ParseError_RejectedNotDegraded — a malformed payload
// must surface as `invalid_sql`, never silently bypass the gate.
func TestExecuteDBExec_ParseError_RejectedNotDegraded(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// validateMutationOnly lets this through (leading word is UPDATE, no
	// banned keywords); only the parser catches it.
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"UPDATE tickets SET (((", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// -----------------------------------------------------------------------------
// db_query — RangeFunction cross-schema gate.
// -----------------------------------------------------------------------------

// TestExecuteDBQuery_RangeFunctionCrossSchema_Denied — function-as-table
// (`SELECT * FROM other_addon.my_func()`) was not gated before v0.10.3
// because libpg_query exposes FuncCall separately from RangeVar. The walker
// now reaches into RangeFunction.Functions and pulls the schema-qualified
// function name out so it's gated like any other relation reference.
func TestExecuteDBQuery_RangeFunctionCrossSchema_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"SELECT * FROM other_addon.my_func(1)", nil)

	env := unmarshalExec(t, out)
	if env.Success {
		t.Fatalf("expected forbidden, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "other_addon.my_func") {
		t.Errorf("expected message to mention other_addon.my_func, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBQuery_RangeFunctionWithCap_Allowed — same query, but with the
// explicit `db:read other_addon.my_func` capability declared.
func TestExecuteDBQuery_RangeFunctionWithCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT \* FROM other_addon.my_func`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:read", Target: "other_addon.my_func"}}
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", caps),
		"SELECT * FROM other_addon.my_func(1)", nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBQuery_RangeFunctionBareNameAllowed — bare function name
// resolves through the search_path the same way bare table names do; no
// extra capability needed.
func TestExecuteDBQuery_RangeFunctionBareNameAllowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT \* FROM my_func`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"SELECT * FROM my_func(1)", nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// -----------------------------------------------------------------------------
// db_exec — RangeFunction inside a mutation gates BOTH caps.
// -----------------------------------------------------------------------------

// TestExecuteDBExec_RangeFunctionInsideInsert_GatesBothCaps — a function-as-
// table reached from within an INSERT body must trip the gate on both axes:
// missing either db:read or db:write for the function denies the call,
// because a setof-returning function may read AND write state.
func TestExecuteDBExec_RangeFunctionInsideInsert_OnlyReadDeclared_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	caps := []manifest.Capability{{Kind: "db:read", Target: "other_addon.iterate"}}
	sql := `INSERT INTO tickets (uid)
			SELECT id FROM other_addon.iterate()`
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", caps), sql, nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden (missing db:write on function), got %s", out)
	}
	if !strings.Contains(env.Error.Message, "other_addon.iterate") {
		t.Errorf("expected message to mention other_addon.iterate, got %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "db:write") {
		t.Errorf("expected message to mention db:write, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_RangeFunctionInsideInsert_BothCapsDeclared_Allowed —
// declaring both caps lets the call through.
func TestExecuteDBExec_RangeFunctionInsideInsert_BothCapsDeclared_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO tickets`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	caps := []manifest.Capability{
		{Kind: "db:read", Target: "other_addon.iterate"},
		{Kind: "db:write", Target: "other_addon.iterate"},
	}
	sql := `INSERT INTO tickets (uid)
			SELECT id FROM other_addon.iterate()`
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcerWithCaps("tickets", caps), sql, nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
