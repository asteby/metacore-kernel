package wasm

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// enforcerWithCaps returns an Enforcer wired to a fixed capability list for
// the given addon key. Mirrors the host's `host.App` wiring and exercises
// the same code path security.Compile builds at install time.
func enforcerWithCaps(addonKey string, caps []manifest.Capability) *security.Enforcer {
	compiled := security.Compile(addonKey, caps)
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		if k == addonKey {
			return compiled
		}
		return nil
	})
	e.SetMode(security.ModeEnforce)
	return e
}

// -----------------------------------------------------------------------------
// extractRelations — AST-level unit tests (no DB needed).
// -----------------------------------------------------------------------------

func TestExtractRelations_BareName(t *testing.T) {
	rels, err := extractRelations("SELECT id FROM tickets")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "", Table: "tickets", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_SchemaQualified(t *testing.T) {
	rels, err := extractRelations("SELECT * FROM public.users")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "public", Table: "users", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_CrossSchemaJoin(t *testing.T) {
	sql := `SELECT a.id, b.name, c.amount
			  FROM addon_tickets.a
			  JOIN public.users b ON b.id = a.uid
			  JOIN billing.invoices c ON c.user_id = b.id`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "addon_tickets", Table: "a", Cap: capRead},
		{Schema: "billing", Table: "invoices", Cap: capRead},
		{Schema: "public", Table: "users", Cap: capRead},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_CTEInternalReferences(t *testing.T) {
	sql := `WITH recent AS (
				SELECT id FROM public.users WHERE created_at > now() - interval '1 day'
			)
			SELECT * FROM recent`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "", Table: "recent", Cap: capRead},
		{Schema: "public", Table: "users", Cap: capRead},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_SubqueryInWhere(t *testing.T) {
	sql := `SELECT id FROM tickets WHERE assignee IN (SELECT id FROM public.users)`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "", Table: "tickets", Cap: capRead},
		{Schema: "public", Table: "users", Cap: capRead},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_DerivedTable(t *testing.T) {
	sql := `SELECT * FROM (SELECT id FROM public.users) sub`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{{Schema: "public", Table: "users", Cap: capRead}}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_UnionArms(t *testing.T) {
	sql := `SELECT id FROM addon_tickets.a UNION ALL SELECT id FROM public.audit`
	rels, err := extractRelations(sql)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []relationRef{
		{Schema: "addon_tickets", Table: "a", Cap: capRead},
		{Schema: "public", Table: "audit", Cap: capRead},
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("got %#v want %#v", rels, want)
	}
}

func TestExtractRelations_RejectsParseError(t *testing.T) {
	_, err := extractRelations("SELECT FROM WHERE")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// -----------------------------------------------------------------------------
// executeDBQuery — cross-schema gate integration tests with sqlmock.
// -----------------------------------------------------------------------------

// TestExecuteDBQuery_BareNameAllowed asserts the v0.10.1 path is unchanged:
// a query against the addon's own implicit schema still works without any
// extra capability declaration.
func TestExecuteDBQuery_BareNameAllowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM tickets`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"SELECT id FROM tickets", nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBQuery_OwnSchemaQualifiedAllowed — explicitly addressing the
// addon's own schema must work just as well as the bare form.
func TestExecuteDBQuery_OwnSchemaQualifiedAllowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM addon_tickets.tickets`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"SELECT id FROM addon_tickets.tickets", nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBQuery_CrossSchemaWithoutCap_Denied is the regression for
// hallazgo #6 of the ABI v1 audit. Before v0.10.2 this query reached the
// driver and read other-schema data because `SET LOCAL search_path` only
// scoped *bare* names. With AST gating in place it must be denied.
func TestExecuteDBQuery_CrossSchemaWithoutCap_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"SELECT * FROM public.users", nil)

	env := unmarshal(t, out)
	if env.Success {
		t.Fatalf("expected forbidden, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected code=forbidden, got %#v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "public.users") {
		t.Errorf("expected message to mention public.users, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBQuery_CrossSchemaWithStarCap_Allowed — same query, but now
// the addon manifest declares `db:read public.*` so the relation passes.
func TestExecuteDBQuery_CrossSchemaWithStarCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT \* FROM public.users`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:read", Target: "public.*"}}
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", caps),
		"SELECT * FROM public.users", nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBQuery_CrossSchemaWithTableCap_Allowed — narrower grant
// (single relation) must also pass.
func TestExecuteDBQuery_CrossSchemaWithTableCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM public.users`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	caps := []manifest.Capability{{Kind: "db:read", Target: "public.users"}}
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", caps),
		"SELECT id FROM public.users", nil)

	env := unmarshal(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteDBQuery_MixedJoin_PartialCap_Denied — capability for one
// relation must not implicitly grant the join's other relation.
func TestExecuteDBQuery_MixedJoin_PartialCap_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	caps := []manifest.Capability{{Kind: "db:read", Target: "public.*"}}
	sql := `SELECT a.id, u.name, i.amount
			  FROM addon_tickets.a
			  JOIN public.users u ON u.id = a.uid
			  JOIN billing.invoices i ON i.user_id = u.id`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", caps), sql, nil)

	env := unmarshal(t, out)
	if env.Success {
		t.Fatalf("expected forbidden, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "billing.invoices") {
		t.Errorf("expected message to mention billing.invoices, got %q", env.Error.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBQuery_MixedJoin_FullCap_Allowed — same join, with grants for
// both cross-schema relations.
func TestExecuteDBQuery_MixedJoin_FullCap_Allowed(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_tickets", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT .* FROM addon_tickets.a`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit()

	caps := []manifest.Capability{
		{Kind: "db:read", Target: "public.*"},
		{Kind: "db:read", Target: "billing.invoices"},
	}
	sql := `SELECT a.id
			  FROM addon_tickets.a
			  JOIN public.users u ON u.id = a.uid
			  JOIN billing.invoices i ON i.user_id = u.id`
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

// TestExecuteDBQuery_CTECrossSchema_Denied — the cross-schema reference is
// hidden behind a CTE body; pre-fix this slipped past the gate because the
// outer FROM only mentioned the CTE name `recent`.
func TestExecuteDBQuery_CTECrossSchema_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `WITH recent AS (
				SELECT id FROM public.users
			)
			SELECT * FROM recent`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshal(t, out)
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

// TestExecuteDBQuery_SubqueryCrossSchema_Denied — same shape as the CTE
// case, but the cross-schema reference lives in an IN(SELECT …) SubLink.
func TestExecuteDBQuery_SubqueryCrossSchema_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	sql := `SELECT id FROM tickets
				WHERE assignee IN (SELECT id FROM public.users)`
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil), sql, nil)

	env := unmarshal(t, out)
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

// TestExecuteDBQuery_DerivedTableCrossSchema_Denied — `FROM (SELECT … FROM
// public.users) sub` must trip the gate at the RangeSubselect node.
func TestExecuteDBQuery_DerivedTableCrossSchema_Denied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		`SELECT * FROM (SELECT id FROM public.users) sub`, nil)

	env := unmarshal(t, out)
	if env.Success {
		t.Fatalf("expected forbidden, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %#v", env.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

// TestExecuteDBQuery_ParseError_RejectedNotDegraded — a malformed payload
// must surface as `invalid_sql`, never silently bypass the gate.
func TestExecuteDBQuery_ParseError_RejectedNotDegraded(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// validateSelectOnly lets this through (leading word is SELECT, no
	// banned keywords); only the parser catches it.
	out := executeDBQuery(context.Background(), gdb, "tickets",
		enforcerWithCaps("tickets", nil),
		"SELECT * FROM (((", nil)

	env := unmarshal(t, out)
	if env.Success {
		t.Fatalf("expected invalid_sql, got success: %s", out)
	}
	if env.Error == nil || env.Error.Code != "invalid_sql" {
		t.Fatalf("expected invalid_sql, got %#v", env.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}
