package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// suEnvelope is the wire shape of a failed data_* import including the
// source_unavailable extras (DISENO-DEGRADACION §B.1).
type suEnvelope struct {
	Success bool `json:"success"`
	Error   struct {
		Code       string `json:"code"`
		LegacyCode string `json:"legacy_code"`
		Message    string `json:"message"`
		MessageKey string `json:"message_key"`
		Details    *struct {
			Source          string   `json:"source"`
			Reason          string   `json:"reason"`
			Owners          []string `json:"owners"`
			InstalledOwners []string `json:"installed_owners"`
			SQLState        string   `json:"sqlstate"`
			Degradable      bool     `json:"degradable"`
		} `json:"details"`
	} `json:"error"`
}

func decodeSU(t *testing.T, raw []byte) suEnvelope {
	t.Helper()
	var e suEnvelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("unmarshal: %v -- %s", err, raw)
	}
	return e
}

func pgErr(code, msg string) error {
	return &pgconn.PgError{Severity: "ERROR", Code: code, Message: msg}
}

// The classification table of DG1.1: SQLSTATE x ownership -> code/reason.
func TestSourceUnavailable_ClassificationTable(t *testing.T) {
	missing := pgErr("42P01", `relation "fiscal_documents" does not exist`)
	cases := []struct {
		name       string
		err        error
		info       *SourceInfo // nil = no WithSourceOwners
		wantCode   string
		wantReason string
	}{
		{"42P01 sin resolver de dueños", missing, nil, CodeSourceUnavailable, ReasonTableMissing},
		{"42P01 dueño desconocido", missing, &SourceInfo{}, CodeSourceUnavailable, ReasonTableMissing},
		{"42P01 dueño NO instalado", missing, &SourceInfo{Owners: []string{"fiscal_mexico"}}, CodeSourceUnavailable, ReasonAddonNotInstalled},
		{"42P01 dueño deshabilitado", missing, &SourceInfo{Owners: []string{"fiscal_mexico"}, Disabled: []string{"fiscal_mexico"}}, CodeSourceUnavailable, ReasonAddonDisabled},
		{"42P01 dueño instalado = anomalía", missing, &SourceInfo{Owners: []string{"fiscal_mexico"}, Installed: []string{"fiscal_mexico"}}, CodeSourceUnavailable, ReasonTableMissing},
		{"42P01 un dueño instalado, otro no", missing, &SourceInfo{Owners: []string{"fiscal_mexico", "billing-sat"}, Installed: []string{"billing-sat"}}, CodeSourceUnavailable, ReasonTableMissing},
		{"42703 columna inexistente", pgErr("42703", `column "x" does not exist`), nil, CodeSourceUnavailable, ReasonSchemaMismatch},
		{"42P01 envuelto con %w", fmt.Errorf("probe: %w", missing), nil, CodeSourceUnavailable, ReasonTableMissing},
		// Transitorios y ajenos a la ausencia: NO se clasifican.
		{"57014 timeout", pgErr("57014", "canceling statement due to statement timeout"), nil, "db_error", ""},
		{"08006 conexión caída", pgErr("08006", "connection failure"), nil, "db_error", ""},
		{"42501 permiso denegado", pgErr("42501", "permission denied for table fiscal_documents"), nil, "db_error", ""},
		{"23505 unique", pgErr("23505", "duplicate key"), nil, "db_error", ""},
		{"error plano sin SQLSTATE", errors.New("driver: bad connection"), nil, "db_error", ""},
		// Nunca por texto: el mensaje dice 42P01 pero no hay PgError.
		{"texto 42P01 sin PgError no cuenta", errors.New(`relation "x" does not exist (SQLSTATE 42P01)`), nil, "db_error", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gdb, mock, cleanup := newMockGorm(t)
			defer cleanup()
			orgID := uuid.New()
			mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" LIMIT 0`).WillReturnError(tc.err)
			inv := testInvocation(gdb, nil, orgID, fiscalReadEnforcer(), nil)
			if tc.info != nil {
				info := *tc.info
				inv.sourceOwners = func(o uuid.UUID, table string) SourceInfo {
					if o != orgID || table != "fiscal_documents" {
						t.Errorf("resolver got (%s,%s)", o, table)
					}
					return info
				}
			}
			out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents"}`))
			e := decodeSU(t, out)
			if e.Success || e.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q: %s", e.Error.Code, tc.wantCode, out)
			}
			// El texto original del driver queda INTACTO en message.
			if e.Error.Message != tc.err.Error() {
				t.Fatalf("message = %q, want %q", e.Error.Message, tc.err.Error())
			}
			if tc.wantCode == "db_error" {
				if e.Error.Details != nil || e.Error.LegacyCode != "" {
					t.Fatalf("db_error must stay as before (no details/legacy_code): %s", out)
				}
				return
			}
			if e.Error.LegacyCode != "db_error" || e.Error.MessageKey != "wasm.host.error.source_unavailable" {
				t.Fatalf("legacy_code/message_key: %s", out)
			}
			d := e.Error.Details
			if d == nil || d.Reason != tc.wantReason || d.Source != "fiscal_documents" || !d.Degradable {
				t.Fatalf("details = %+v, want reason %q: %s", d, tc.wantReason, out)
			}
			if d.SQLState == "" || d.Owners == nil || d.InstalledOwners == nil {
				t.Fatalf("details must carry sqlstate and non-null owner lists: %s", out)
			}
		})
	}
}

// Un fallo en el SELECT real (no en el probe) también se clasifica: un filtro
// sobre una columna que el dueño aún no tiene es schema_mismatch.
func TestSourceUnavailable_SelectUndefinedColumn(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
	mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnError(pgErr("42703", `column "uuid" does not exist`))
	inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","where":{"uuid":"x"}}`))
	e := decodeSU(t, out)
	if e.Error.Code != CodeSourceUnavailable || e.Error.Details.Reason != ReasonSchemaMismatch {
		t.Fatalf("want schema_mismatch: %s", out)
	}
}

// El resolver de dueños se consulta una sola vez por tabla y por invocación.
func TestSourceUnavailable_OwnersResolvedOncePerInvocation(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	for i := 0; i < 3; i++ {
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" LIMIT 0`).WillReturnError(pgErr("42P01", "x"))
	}
	inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
	calls := 0
	inv.sourceOwners = func(uuid.UUID, string) SourceInfo { calls++; return SourceInfo{Owners: []string{"fiscal_mexico"}} }
	for i := 0; i < 3; i++ {
		executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents"}`))
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (memoised per invocation)", calls)
	}
}

// Una escritura sobre fuente ausente es SIEMPRE source_unavailable y nunca
// degradable (el host jamás la convierte en no-op).
func TestSourceUnavailable_WriteIsNeverDegradable(t *testing.T) {
	t.Run("data_mutate create", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnError(pgErr("42P01", `relation "stock" does not exist`))
		mock.ExpectRollback()
		inv := testInvocation(gdb, events.NewBus(nil), uuid.New(), stockWriteEnforcer(), nil)
		inv.sourceOwners = func(uuid.UUID, string) SourceInfo { return SourceInfo{Owners: []string{"inventory"}} }
		out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}}`))
		e := decodeSU(t, out)
		if e.Error.Code != CodeSourceUnavailable || e.Error.Details == nil ||
			e.Error.Details.Reason != ReasonAddonNotInstalled || e.Error.Details.Degradable || e.Error.LegacyCode != "db_error" {
			t.Fatalf("write on absent source: %s", out)
		}
	})
	t.Run("data_mutate 42703 en INSERT", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
		mock.ExpectQuery(`INSERT INTO "stock"`).WillReturnError(pgErr("42703", `column "quantity" of relation "stock" does not exist`))
		mock.ExpectRollback()
		inv := testInvocation(gdb, events.NewBus(nil), uuid.New(), stockWriteEnforcer(), nil)
		out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}}`))
		e := decodeSU(t, out)
		if e.Error.Details == nil || e.Error.Details.Reason != ReasonSchemaMismatch || e.Error.Details.Degradable {
			t.Fatalf("42703 write: %s", out)
		}
	})
	t.Run("data_mutate error transitorio sigue db_error", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnError(pgErr("57014", "timeout"))
		mock.ExpectRollback()
		inv := testInvocation(gdb, events.NewBus(nil), uuid.New(), stockWriteEnforcer(), nil)
		out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}}`))
		if e := decodeSU(t, out); e.Error.Code != "db_error" || e.Error.Details != nil {
			t.Fatalf("transient write must stay db_error: %s", out)
		}
	})
	t.Run("data_batch conserva el prefijo mutations[i]", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnError(pgErr("42P01", `relation "stock" does not exist`))
		mock.ExpectRollback()
		inv := testInvocation(gdb, events.NewBus(nil), uuid.New(), multiWriteEnforcer("stock"), nil)
		out := executeDataBatch(context.Background(), inv, []byte(`{"mutations":[{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}}]}`))
		e := decodeSU(t, out)
		if e.Error.Code != CodeSourceUnavailable || !strings.HasPrefix(e.Error.Message, "mutations[0]: ") ||
			!strings.Contains(e.Error.Message, "42P01") && !strings.Contains(e.Error.Message, "does not exist") {
			t.Fatalf("batch: %s", out)
		}
		if e.Error.Details.Degradable {
			t.Fatalf("batch write must not be degradable: %s", out)
		}
	})
}

// fiscalReadEnforcer grants `db:read fiscal_documents`.
func fiscalReadEnforcer() *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{{Kind: "db:read", Target: "fiscal_documents"}})
	})
	e.SetMode(security.ModeEnforce)
	return e
}

// fiscalAndStockReadEnforcer grants db:read on both logical tables.
func fiscalAndStockReadEnforcer() *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{
			{Kind: "db:read", Target: "fiscal_documents"}, {Kind: "db:read", Target: "stock"},
		})
	})
	e.SetMode(security.ModeEnforce)
	return e
}
