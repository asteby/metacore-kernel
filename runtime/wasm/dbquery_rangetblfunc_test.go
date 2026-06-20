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
// extractRelations — AST-level unit tests for RangeTblFunc-class nodes.
//
// These are the v0.11.x follow-up to PR #61 / #63: libpg_query exposes the
// XMLTABLE and JSON_TABLE constructs as their own node types (`RangeTableFunc`
// and `JsonTable`), distinct from `RangeVar` / `RangeFunction`. A subquery
// nested in the `PASSING` clause or column expression of either could read
// from another schema without ever surfacing through the existing walker —
// see § 9.3 / § 10.3 of docs/wasm-abi.md for the contract.
// -----------------------------------------------------------------------------

// TestExtractRelations_XMLTablePassingSubqueryDescends — the cross-schema
// table lives inside an XMLTABLE PASSING subquery. The walker must descend
// into RangeTableFunc.Docexpr → SubLink.Subselect → SelectStmt.from_clause
// to surface it.
func TestExtractRelations_XMLTablePassingSubqueryDescends(t *testing.T) {
	sql := `SELECT * FROM XMLTABLE('//row'
				PASSING (SELECT data FROM other_schema.audit_log)
				COLUMNS id INT PATH 'id')`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "other_schema", Table: "audit_log", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// TestExtractRelations_XMLTableLiteralOnly — XMLTABLE whose PASSING value is
// a pure XML literal references no relations. The walker must NOT manufacture
// a spurious entry.
func TestExtractRelations_XMLTableLiteralOnly(t *testing.T) {
	sql := `SELECT * FROM XMLTABLE('//row'
				PASSING xmlparse(document '<rows><row><id>1</id></row></rows>')
				COLUMNS id INT PATH 'id')`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(rels) != 0 {
		t.Fatalf("expected no relations, got %#v", rels)
	}
}

// TestExtractRelations_XMLTableColumnDefaultSubqueryDescends — the DEFAULT
// expression of an XMLTABLE column is the second hidden vector: it can hold
// a SubLink reading another schema. RangeTableFuncCol carries it on
// Coldefexpr, which the walker must descend through.
func TestExtractRelations_XMLTableColumnDefaultSubqueryDescends(t *testing.T) {
	sql := `SELECT * FROM XMLTABLE('//row'
				PASSING xmlparse(document '<r/>')
				COLUMNS id INT PATH 'id' DEFAULT (SELECT id FROM other.fallback LIMIT 1))`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "other", Table: "fallback", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// TestExtractRelations_JsonTableContextItemSubqueryDescends — the context
// item of a JSON_TABLE is wrapped in a JsonValueExpr whose RawExpr is the
// actual source. A subquery there must surface through the gate.
func TestExtractRelations_JsonTableContextItemSubqueryDescends(t *testing.T) {
	sql := `SELECT * FROM JSON_TABLE(
				(SELECT j FROM other.json_src), '$' COLUMNS (id int PATH '$.id'))`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "other", Table: "json_src", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// TestExtractRelations_JsonTableNestedPath — JSON_TABLE supports `NESTED
// PATH '…' COLUMNS (…)` which produces a JsonTableColumn whose own Columns
// list carries the children. The walker must recurse so a subquery hidden
// in the nested branch still trips the gate.
func TestExtractRelations_JsonTableNestedPath(t *testing.T) {
	sql := `SELECT * FROM JSON_TABLE(
				'[1,2]'::jsonb,
				'$[*]' COLUMNS (
					id int PATH '$',
					NESTED PATH '$.kids[*]' COLUMNS (
						kid int PATH '$' DEFAULT (SELECT id FROM other.nested LIMIT 1) ON EMPTY
					)
				))`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "other", Table: "nested", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// TestExtractRelations_JsonTablePassingArgumentSubqueryDescends — the
// PASSING clause of a JSON_TABLE attaches named JsonArgument nodes whose
// Val is a JsonValueExpr. The walker has to descend through both to reach
// any nested cross-schema read.
func TestExtractRelations_JsonTablePassingArgumentSubqueryDescends(t *testing.T) {
	sql := `SELECT * FROM JSON_TABLE(
				'{"a":1}'::jsonb,
				'$' PASSING (SELECT j FROM other.params) AS p
				COLUMNS (id int PATH '$.a'))`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "other", Table: "params", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// TestExtractMutationRelations_XMLTableInsideInsert_PreservesSurroundingCap —
// when an XMLTABLE is reached from inside a DML's SELECT body, the
// cross-schema relation referenced in its PASSING subquery is gated under
// capRead (the surrounding SELECT scope), and the DML target keeps capWrite.
// Unlike RangeFunction, XMLTABLE is a SQL construct (not a callable
// function) so the inMutation defensive both-axes gate does NOT apply.
func TestExtractMutationRelations_XMLTableInsideInsert_PreservesSurroundingCap(t *testing.T) {
	sql := `INSERT INTO tickets (id)
			SELECT t.id FROM XMLTABLE('//r'
				PASSING (SELECT data FROM other.src)
				COLUMNS id INT PATH 'id') AS t`
	rels, err := extractMutationRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "other", Table: "src", Cap: capRead},
		{Schema: "", Table: "tickets", Cap: capWrite},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

// -----------------------------------------------------------------------------
// executeDBQuery — RangeTblFunc cross-schema integration tests.
// -----------------------------------------------------------------------------

// TestExecuteDBQuery_XMLTablePassingCrossSchema_Denied — full-stack
// regression: the PR #61 follow-up vector slipped past the walker because
// XMLTABLE produces a RangeTableFunc, not a RangeVar / RangeFunction. The
// host must now reject the call before the driver is touched.
func TestExecuteDBQuery_XMLTablePassingCrossSchema_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `SELECT * FROM XMLTABLE('//row'
				PASSING (SELECT data FROM other.audit_log)
				COLUMNS id INT PATH 'id')`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshal(t, out)
	if env.Success {
		t.Fatalf("expected forbidden, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "other.audit_log") {
		t.Errorf("expected message to mention other.audit_log, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBQuery_XMLTablePassingCrossSchema_WithCap_Allowed — declaring
// db:read on the cross-schema table lets the XMLTABLE call through.
func TestExecuteDBQuery_XMLTablePassingCrossSchema_WithCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`XMLTABLE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:read", Target: "other.audit_log"}}
	sql := `SELECT * FROM XMLTABLE('//row'
				PASSING (SELECT data FROM other.audit_log)
				COLUMNS id INT PATH 'id')`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", caps), sql, nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBQuery_XMLTableLiteralOnly_BareAllowed — XMLTABLE whose
// PASSING value is a pure literal references no relations, so the call
// goes through under the implicit own-schema grant alone.
func TestExecuteDBQuery_XMLTableLiteralOnly_BareAllowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`XMLTABLE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	sql := `SELECT * FROM XMLTABLE('//row'
				PASSING xmlparse(document '<rows><row><id>1</id></row></rows>')
				COLUMNS id INT PATH 'id')`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBQuery_JsonTablePassingCrossSchema_Denied — JSON_TABLE
// counterpart of the XMLTABLE case above.
func TestExecuteDBQuery_JsonTablePassingCrossSchema_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `SELECT * FROM JSON_TABLE(
				(SELECT j FROM other.json_src), '$' COLUMNS (id int PATH '$.id'))`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshal(t, out)
	if env.Success {
		t.Fatalf("expected forbidden, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "other.json_src") {
		t.Errorf("expected message to mention other.json_src, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBQuery_JsonTablePassingCrossSchema_WithCap_Allowed — same
// JSON_TABLE call, with the declared capability.
func TestExecuteDBQuery_JsonTablePassingCrossSchema_WithCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`JSON_TABLE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:read", Target: "other.json_src"}}
	sql := `SELECT * FROM JSON_TABLE(
				(SELECT j FROM other.json_src), '$' COLUMNS (id int PATH '$.id'))`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", caps), sql, nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// -----------------------------------------------------------------------------
// executeDBExec — RangeTblFunc inside a DML body.
// -----------------------------------------------------------------------------

// TestExecuteDBExec_XMLTableInsideInsert_Denied — the cross-schema read is
// reached from inside an INSERT … SELECT … XMLTABLE(…). It must be gated
// under db:read just like the SELECT-only case.
func TestExecuteDBExec_XMLTableInsideInsert_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `INSERT INTO tickets (id)
			SELECT t.id FROM XMLTABLE('//r'
				PASSING (SELECT data FROM other.src)
				COLUMNS id INT PATH 'id') AS t`
	out := executeDBExec(context.Background(), nil, gdb, "tickets", "",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "other.src") {
		t.Errorf("expected message to mention other.src, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBExec_XMLTableInsideInsert_WithReadCap_Allowed — declaring
// db:read on the cross-schema source (the implicit own-schema db:write
// covers the INSERT target) lets the call through.
func TestExecuteDBExec_XMLTableInsideInsert_WithReadCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO tickets`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	caps := []manifest.Capability{{Kind: "db:read", Target: "other.src"}}
	sql := `INSERT INTO tickets (id)
			SELECT t.id FROM XMLTABLE('//r'
				PASSING (SELECT data FROM other.src)
				COLUMNS id INT PATH 'id') AS t`
	out := executeDBExec(context.Background(), gdb, nil, "tickets", "",
		enforcerWithCaps("tickets", caps), sql, nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
