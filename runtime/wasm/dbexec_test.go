package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/security"
)

// dbeEnvelope is the db_exec wire shape (rowsAffected lives on data, not
// rows/columns like db_query). Reusing dbqEnvelope would silently drop the
// field and the tests would pass without verifying it.
type dbeEnvelope struct {
	Success bool           `json:"success"`
	Data    *dbeData       `json:"data,omitempty"`
	Error   *dbqError      `json:"error,omitempty"`
	Meta    map[string]any `json:"meta"`
}

type dbeData struct {
	RowsAffected int64 `json:"rowsAffected"`
}

func unmarshalExec(t *testing.T, raw []byte) dbeEnvelope {
	t.Helper()
	var env dbeEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v -- %s", err, raw)
	}
	return env
}

func TestExecuteDBExec_HappyPathTx(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// When the host enters via InvokeInTx the surrounding action handler
	// already opened the transaction. The host MUST NOT issue its own
	// Begin/Commit on this path — sqlmock asserts that by failing the
	// expectations check if either one is observed without being declared.
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO tickets`).
		WithArgs("hello", "open").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := executeDBExec(context.Background(), gdb, nil, "tickets",
		permissiveEnforcer(),
		"INSERT INTO tickets (title, status) VALUES ($1, $2)",
		[]byte(`["hello", "open"]`))

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data == nil || env.Data.RowsAffected != 1 {
		t.Fatalf("expected rowsAffected=1, got %#v", env.Data)
	}
	if env.Meta["schema"] != "addon_tickets" {
		t.Fatalf("expected schema addon_tickets, got %v", env.Meta["schema"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDBExec_HappyPathStandalone(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// No tx provided → host opens its own short-lived transaction.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE tickets SET status = \$1 WHERE id = \$2`).
		WithArgs("closed", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		permissiveEnforcer(),
		"UPDATE tickets SET status = $1 WHERE id = $2",
		[]byte(`["closed", 7]`))

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data == nil || env.Data.RowsAffected != 1 {
		t.Fatalf("expected rowsAffected=1, got %#v", env.Data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDBExec_RejectsSelect(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		permissiveEnforcer(),
		"SELECT * FROM tickets", nil)

	env := unmarshalExec(t, out)
	if env.Success {
		t.Fatal("expected failure for SELECT")
	}
	if env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql, got %#v", env.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDBExec_RejectsMultiStatement(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		permissiveEnforcer(),
		"INSERT INTO tickets DEFAULT VALUES; DELETE FROM tickets", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql, got %s", out)
	}
}

func TestExecuteDBExec_RejectsIntrospection(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		permissiveEnforcer(),
		"DELETE FROM information_schema.tables", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql for information_schema, got %s", out)
	}
}

func TestExecuteDBExec_RejectsBannedKeyword(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	// DROP wrapped behind a leading INSERT — the leading-word check passes
	// but the banned-keyword scan must still catch DROP.
	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		permissiveEnforcer(),
		"INSERT INTO tickets DEFAULT VALUES; DROP TABLE tickets", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql for DROP, got %s", out)
	}
}

func TestExecuteDBExec_LiteralWithKeyword(t *testing.T) {
	// A literal containing a banned keyword must NOT trip validation —
	// e.g. an INSERT that stores the word 'DROP' as a plain string is fine.
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO tickets`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := executeDBExec(context.Background(), gdb, nil, "tickets",
		permissiveEnforcer(),
		"INSERT INTO tickets (note) VALUES ('DROP me')", nil)

	env := unmarshalExec(t, out)
	if !env.Success {
		t.Fatalf("expected success for INSERT with banned-word literal, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestExecuteDBExec_CapabilityDenied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// Lookup returns nil → addon "tickets" is treated as unregistered, so
	// CheckCapability surfaces a violation in enforce mode.
	denying := security.NewEnforcer(func(string) *security.Capabilities { return nil })
	denying.SetMode(security.ModeEnforce)

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		denying, "INSERT INTO tickets DEFAULT VALUES", nil)

	env := unmarshalExec(t, out)
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

func TestExecuteDBExec_CapabilityMissingDbWrite(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// Addon registered but without the implicit own-schema grant — exercises
	// the message path that surfaces "lacks db:write …".
	enforcer := security.NewEnforcer(func(k string) *security.Capabilities {
		// Compile a stand-in addon under a different key so its implicit
		// addon_other.* grant doesn't cover addon_tickets.*.
		return security.Compile("other", nil)
	})
	enforcer.SetMode(security.ModeEnforce)

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		enforcer, "INSERT INTO tickets DEFAULT VALUES", nil)

	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "db:write") {
		t.Fatalf("expected message to mention db:write, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDBExec_DBUnavailable(t *testing.T) {
	out := executeDBExec(context.Background(), nil, nil, "tickets",
		permissiveEnforcer(), "INSERT INTO tickets DEFAULT VALUES", nil)
	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "db_unavailable" {
		t.Fatalf("expected db_unavailable, got %s", out)
	}
}

func TestExecuteDBExec_DriverError(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO tickets`).WillReturnError(sqlErr("kaboom"))
	mock.ExpectRollback()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		permissiveEnforcer(),
		"INSERT INTO tickets DEFAULT VALUES", nil)
	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "db_error" {
		t.Fatalf("expected db_error, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "kaboom") {
		t.Fatalf("expected message to surface 'kaboom', got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestExecuteDBExec_BadArgs(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBExec(context.Background(), nil, gdb, "tickets",
		permissiveEnforcer(),
		"INSERT INTO tickets DEFAULT VALUES",
		[]byte(`{"not":"an array"}`))
	env := unmarshalExec(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "arg_decode" {
		t.Fatalf("expected arg_decode, got %s", out)
	}
}

func TestValidateMutationOnly(t *testing.T) {
	good := []string{
		"INSERT INTO tickets DEFAULT VALUES",
		"insert into tickets (id) values (1)",
		"UPDATE tickets SET status = 'closed' WHERE id = 1",
		"DELETE FROM tickets WHERE id = 1",
		"MERGE INTO tickets USING staging ON tickets.id = staging.id WHEN MATCHED THEN UPDATE SET status = 'x'",
		"INSERT INTO tickets (note) VALUES ('DROP me');",
		"INSERT INTO tickets (note) SELECT note FROM staging",
	}
	for _, s := range good {
		if err := validateMutationOnly(s); err != nil {
			t.Errorf("validateMutationOnly(%q) unexpected err: %v", s, err)
		}
	}
	bad := []string{
		"",
		";",
		"SELECT 1",
		"WITH t AS (SELECT 1) SELECT * FROM t",
		"DROP TABLE tickets",
		"TRUNCATE tickets",
		"INSERT INTO tickets DEFAULT VALUES; DELETE FROM tickets",
		"INSERT INTO tickets DEFAULT VALUES; DROP TABLE tickets",
		"BEGIN",
		"COMMIT",
		"SAVEPOINT foo",
		"DELETE FROM information_schema.tables",
		"DELETE FROM pg_catalog.pg_class",
	}
	for _, s := range bad {
		if err := validateMutationOnly(s); err == nil {
			t.Errorf("validateMutationOnly(%q) should have failed", s)
		}
	}
}

