package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/security"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newMockGorm wires sqlmock behind a gorm.DB driving the postgres dialect
// in simple-protocol mode. SkipDefaultTransaction is critical: gorm
// otherwise wraps each Raw() in its own auto-tx and our explicit Begin /
// SET LOCAL would race with that wrapping.
func newMockGorm(t *testing.T) (*gorm.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	gdb, err := gorm.Open(postgres.New(postgres.Config{
		Conn:                 sqlDB,
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		SkipDefaultTransaction: true,
	})
	if err != nil {
		_ = sqlDB.Close()
		t.Fatalf("gorm open: %v", err)
	}
	return gdb, mock, func() { _ = sqlDB.Close() }
}

type dbqEnvelope struct {
	Success bool             `json:"success"`
	Data    *dbqData         `json:"data,omitempty"`
	Error   *dbqError        `json:"error,omitempty"`
	Meta    map[string]any   `json:"meta"`
}

type dbqData struct {
	Rows     []map[string]any   `json:"rows"`
	RowCount int                `json:"rowCount"`
	Columns  []map[string]any   `json:"columns"`
}

type dbqError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func unmarshal(t *testing.T, raw []byte) dbqEnvelope {
	t.Helper()
	var env dbqEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v -- %s", err, raw)
	}
	return env
}

// permissiveEnforcer returns an Enforcer whose lookup grants the addon its
// own implicit `addon_<key>.*` capability and nothing more — exactly what
// security.Compile produces for a manifest with empty capabilities.
func permissiveEnforcer() *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, nil)
	})
	e.SetMode(security.ModeEnforce)
	return e
}

func TestExecuteDBQuery_HappyPath(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id, title FROM tickets WHERE status = \$1`).
		WithArgs("open").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title"}).
			AddRow(int64(1), "first").
			AddRow(int64(2), "second"))
	mock.ExpectCommit()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(),
		"SELECT id, title FROM tickets WHERE status = $1",
		[]byte(`["open"]`))

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data == nil || env.Data.RowCount != 2 {
		t.Fatalf("expected 2 rows, got %#v", env.Data)
	}
	if env.Meta["schema"] != "addon_tickets" {
		t.Fatalf("expected schema addon_tickets, got %v", env.Meta["schema"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDBQuery_RejectsMutation(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(),
		"DELETE FROM tickets WHERE id = $1", []byte(`[1]`))

	env := unmarshal(t, out)
	if env.Success {
		t.Fatal("expected failure for DELETE")
	}
	if env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql, got %#v", env.Error)
	}
	// The driver must not have been touched.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDBQuery_RejectsMultiStatement(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(),
		"SELECT 1; SELECT 2", nil)

	env := unmarshal(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql, got %s", out)
	}
}

func TestExecuteDBQuery_RejectsIntrospection(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(),
		"SELECT * FROM information_schema.tables", nil)

	env := unmarshal(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql for information_schema, got %s", out)
	}
}

func TestExecuteDBQuery_LiteralWithKeyword(t *testing.T) {
	// A literal containing a banned keyword must NOT trip validation —
	// e.g. searching for the string 'DELETE me' is legitimate.
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM tickets WHERE title = '.*'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectCommit()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(),
		"SELECT id FROM tickets WHERE title = 'DELETE me'", nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestExecuteDBQuery_CapabilityDenied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// Lookup returns nil → addon "tickets" is treated as unregistered, so
	// CheckCapability surfaces a violation in enforce mode.
	denying := security.NewEnforcer(func(string) *security.Capabilities { return nil })
	denying.SetMode(security.ModeEnforce)

	out := executeDBQuery(context.Background(), gdb, "tickets",
		denying, "SELECT * FROM tickets", nil)

	env := unmarshal(t, out)
	if env.Success {
		t.Fatal("expected forbidden")
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDBQuery_DBUnavailable(t *testing.T) {
	out := executeDBQuery(context.Background(), nil, "tickets",
		permissiveEnforcer(), "SELECT 1", nil)
	env := unmarshal(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "db_unavailable" {
		t.Fatalf("expected db_unavailable, got %s", out)
	}
}

func TestExecuteDBQuery_DriverError(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT 1`).WillReturnError(sqlErr("boom"))
	mock.ExpectRollback()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(), "SELECT 1", nil)
	env := unmarshal(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "db_error" {
		t.Fatalf("expected db_error, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "boom") {
		t.Fatalf("expected message to surface 'boom', got %q", env.Error.Message)
	}
}

func TestExecuteDBQuery_BadArgs(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(),
		"SELECT 1", []byte(`{"not":"an array"}`))
	env := unmarshal(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "arg_decode" {
		t.Fatalf("expected arg_decode, got %s", out)
	}
}

func TestExecuteDBQuery_TypedArgs(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM tickets WHERE id = \$1`).
		WithArgs(sqlmock.AnyArg()). // uuid arg — driver-level type check is sqlmock's job, not ours
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	args := `[{"$uuid":"550e8400-e29b-41d4-a716-446655440000"}]`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		permissiveEnforcer(),
		"SELECT id FROM tickets WHERE id = $1", []byte(args))
	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
}

func TestValidateSelectOnly(t *testing.T) {
	good := []string{
		"SELECT 1",
		"  select * from tickets",
		"WITH t AS (SELECT 1) SELECT * FROM t",
		"SELECT id FROM tickets WHERE title = 'DELETE me'",
		"select * from tickets;",
	}
	for _, s := range good {
		if err := validateSelectOnly(s); err != nil {
			t.Errorf("validateSelectOnly(%q) unexpected err: %v", s, err)
		}
	}
	bad := []string{
		"",
		";",
		"INSERT INTO tickets VALUES (1)",
		"DROP TABLE tickets",
		"SELECT 1; SELECT 2",
		"SET search_path = public",
		"SELECT * FROM pg_catalog.pg_class",
		"SELECT * FROM information_schema.tables",
	}
	for _, s := range bad {
		if err := validateSelectOnly(s); err == nil {
			t.Errorf("validateSelectOnly(%q) should have failed", s)
		}
	}
}

// sqlErr is a tiny helper so tests don't need to import errors.New.
type stringErr string

func (e stringErr) Error() string { return string(e) }

func sqlErr(s string) error { return stringErr(s) }
