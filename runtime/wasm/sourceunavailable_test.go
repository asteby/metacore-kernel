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

// Solo el probe de la tabla PEDIDA prueba su ausencia. Un 42P01 posterior en la
// misma llamada nombra OTRA relación (tabla de extensión en EXISTS/JOIN, un
// trigger): atribuirlo a la tabla pedida inventaría una ausencia degradable. Se
// queda en el db_error de siempre, con el texto (y su 42P01) intacto.
func TestSourceUnavailable_42P01AfterProbeIsNotTheRequestedTable(t *testing.T) {
	t.Run("data_query: SELECT tras probe OK", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
		ext := pgErr("42P01", `relation "fiscal_documents_ext" does not exist`)
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnError(ext)
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		calls := 0
		inv.sourceOwners = func(uuid.UUID, string) SourceInfo { calls++; return SourceInfo{Owners: []string{"fiscal_mexico"}} }
		out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents"}`))
		e := decodeSU(t, out)
		if e.Error.Code != "db_error" || e.Error.Details != nil || e.Error.LegacyCode != "" || e.Error.Message != ext.Error() {
			t.Fatalf("42P01 of another relation must stay db_error: %s", out)
		}
		if calls != 0 {
			t.Fatalf("owners resolver must not run for a non-absence (calls=%d)", calls)
		}
	})
	t.Run("data_mutate: INSERT tras probe OK (trigger/extensión)", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
		mock.ExpectQuery(`INSERT INTO "stock"`).WillReturnError(pgErr("42P01", `relation "stock_audit" does not exist`))
		mock.ExpectRollback()
		inv := testInvocation(gdb, events.NewBus(nil), uuid.New(), stockWriteEnforcer(), nil)
		out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}}`))
		if e := decodeSU(t, out); e.Success || e.Error.Code != "db_error" || e.Error.Details != nil {
			t.Fatalf("42P01 after the probe must stay db_error: %s", out)
		}
	})
}

// data_query no abre savepoint: dentro de la transacción del llamador
// (InvokeInTx) un 42P01 la ABORTA (25P02 en todo lo que sigue). Servir "vacío" y
// seguir no funciona ahí, así que la lectura no es degradable.
func TestSourceUnavailable_ReadInsideTxIsNotDegradable(t *testing.T) {
	tx, mock, cleanup := newMockGormTx(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" LIMIT 0`).WillReturnError(pgErr("42P01", `relation "fiscal_documents" does not exist`))
	inv := testInvocation(nil, nil, uuid.New(), fiscalReadEnforcer(), nil)
	inv.tx = tx
	inv.db = tx
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents"}`))
	e := decodeSU(t, out)
	if e.Error.Code != CodeSourceUnavailable || e.Error.Details == nil || e.Error.Details.Degradable {
		t.Fatalf("read on an aborted shared tx must be source_unavailable, not degradable: %s", out)
	}
}

// ---- DG1.2: on_absent ----------------------------------------------------

type absentOK struct {
	Success bool `json:"success"`
	Data    struct {
		Rows      []map[string]any `json:"rows"`
		Truncated bool             `json:"truncated"`
	} `json:"data"`
	Meta struct {
		Absent *struct {
			Source string   `json:"source"`
			Reason string   `json:"reason"`
			Owners []string `json:"owners"`
		} `json:"absent"`
	} `json:"meta"`
}

func decodeAbsentOK(t *testing.T, raw []byte) absentOK {
	t.Helper()
	var e absentOK
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("unmarshal: %v -- %s", err, raw)
	}
	return e
}

func rawData(t *testing.T, raw []byte) string {
	t.Helper()
	var m struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return string(m.Data)
}

func fiscalOwners(info SourceInfo) func(uuid.UUID, string) SourceInfo {
	return func(uuid.UUID, string) SourceInfo { return info }
}

// Flag ausente / "error" / "empty" / inválido sobre un 42P01 del probe.
func TestOnAbsent_FlagMatrix(t *testing.T) {
	owners := SourceInfo{Owners: []string{"fiscal_mexico"}}
	var runInfo func(t *testing.T, payload string, probeErr bool, owners SourceInfo) []byte
	run := func(t *testing.T, payload string, probeErr bool) []byte {
		return runInfo(t, payload, probeErr, owners)
	}
	runInfo = func(t *testing.T, payload string, probeErr bool, owners SourceInfo) []byte {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		if probeErr {
			mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" LIMIT 0`).WillReturnError(pgErr("42P01", `relation "fiscal_documents" does not exist`))
		}
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		inv.sourceOwners = fiscalOwners(owners)
		out := executeDataQueryRecords(context.Background(), inv, []byte(payload))
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
		return out
	}
	for _, p := range []string{`{"table":"fiscal_documents"}`, `{"table":"fiscal_documents","on_absent":""}`, `{"table":"fiscal_documents","on_absent":"error"}`} {
		e := decodeSU(t, run(t, p, true))
		if e.Success || e.Error.Code != CodeSourceUnavailable || !e.Error.Details.Degradable {
			t.Fatalf("%s: must stay source_unavailable error", p)
		}
	}
	// Owner installed yet the table is missing (half-done install): the install
	// rule does not apply, the probe's 42P01 does -> empty with table_missing.
	out := runInfo(t, `{"table":"fiscal_documents","on_absent":"empty"}`, true, SourceInfo{Owners: []string{"fiscal_mexico"}, Installed: []string{"fiscal_mexico"}})
	e := decodeAbsentOK(t, out)
	if !e.Success || len(e.Data.Rows) != 0 || e.Meta.Absent == nil ||
		e.Meta.Absent.Source != "fiscal_documents" || e.Meta.Absent.Reason != ReasonTableMissing ||
		len(e.Meta.Absent.Owners) != 1 || e.Meta.Absent.Owners[0] != "fiscal_mexico" {
		t.Fatalf("empty: %s", out)
	}
	if d := rawData(t, out); d != `{"rows":[],"truncated":false}` {
		t.Fatalf("data shape: %s", d)
	}
	for _, p := range []string{`{"table":"fiscal_documents","on_absent":"EMPTY"}`, `{"table":"fiscal_documents","on_absent":"skip"}`} {
		e := decodeSU(t, run(t, p, false)) // validated before touching the DB
		if e.Success || e.Error.Code != "invalid_request" {
			t.Fatalf("%s: want invalid_request", p)
		}
	}
}

// data vacío idéntico con y sin aggregate.
func TestOnAbsent_EmptyIdenticalWithAndWithoutAggregate(t *testing.T) {
	var datas []string
	for _, p := range []string{
		`{"table":"fiscal_documents","on_absent":"empty"}`,
		`{"table":"fiscal_documents","on_absent":"empty","aggregate":{"group_by":["status"],"select":[{"fn":"count","as":"n"}]}}`,
	} {
		gdb, mock, cleanup := newMockGorm(t)
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" LIMIT 0`).WillReturnError(pgErr("42P01", "x"))
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		out := executeDataQueryRecords(context.Background(), inv, []byte(p))
		cleanup()
		if e := decodeAbsentOK(t, out); !e.Success || e.Meta.Absent == nil || e.Meta.Absent.Reason != ReasonTableMissing {
			t.Fatalf("%s -> %s", p, out)
		}
		datas = append(datas, rawData(t, out))
	}
	if datas[0] != datas[1] || datas[0] != `{"rows":[],"truncated":false}` {
		t.Fatalf("data differs: %v", datas)
	}
}

// Regla por org: dueños conocidos y ninguno instalado -> ausente SIN tocar la BD
// (el mock no espera ninguna consulta). Sin el flag, la tabla se lee como hoy.
func TestOnAbsent_InstallRuleNeverTouchesDB(t *testing.T) {
	cases := []struct {
		name   string
		info   SourceInfo
		reason string
	}{
		{"no instalado", SourceInfo{Owners: []string{"fiscal_mexico"}}, ReasonAddonNotInstalled},
		{"deshabilitado", SourceInfo{Owners: []string{"fiscal_mexico"}, Disabled: []string{"fiscal_mexico"}}, ReasonAddonDisabled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gdb, mock, cleanup := newMockGorm(t) // sin expectativas: cualquier SQL falla
			defer cleanup()
			inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
			inv.sourceOwners = fiscalOwners(c.info)
			out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","on_absent":"empty"}`))
			e := decodeAbsentOK(t, out)
			if !e.Success || e.Meta.Absent == nil || e.Meta.Absent.Reason != c.reason {
				t.Fatalf("%s", out)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("sin flag: la tabla existente se lee igual que hoy", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		inv.sourceOwners = fiscalOwners(SourceInfo{Owners: []string{"fiscal_mexico"}})
		if e := decodeAbsentOK(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents"}`))); !e.Success || e.Meta.Absent != nil {
			t.Fatal("without the flag the physical read must be unchanged")
		}
	})
	t.Run("dueño instalado o tabla sin catalogar: consulta normal", func(t *testing.T) {
		for _, info := range []SourceInfo{{Owners: []string{"fiscal_mexico"}, Installed: []string{"fiscal_mexico"}}, {}} {
			gdb, mock, cleanup := newMockGorm(t)
			expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
			mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
			inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
			inv.sourceOwners = fiscalOwners(info)
			if e := decodeAbsentOK(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","on_absent":"empty"}`))); !e.Success || e.Meta.Absent != nil {
				t.Fatalf("info %+v must read normally", info)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
			cleanup()
		}
	})
	t.Run("el gate db:read va antes de la regla", func(t *testing.T) {
		gdb, _, cleanup := newMockGorm(t)
		defer cleanup()
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		inv.sourceOwners = fiscalOwners(SourceInfo{Owners: []string{"x"}})
		e := decodeSU(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"stock","on_absent":"empty"}`)))
		if e.Success || e.Error.Code != "forbidden" {
			t.Fatalf("forbidden expected: %+v", e)
		}
	})
}

// 42703 sin tx: degrada con el flag; con tx no degrada. 42501 y 42P01 posterior
// al probe siguen siendo db_error aun con el flag.
func TestOnAbsent_NonAbsencesAndSchemaMismatch(t *testing.T) {
	t.Run("42703 fuera de tx -> vacío schema_mismatch", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnError(pgErr("42703", `column "uuid" does not exist`))
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","where":{"uuid":"x"},"on_absent":"empty"}`))
		if e := decodeAbsentOK(t, out); !e.Success || e.Meta.Absent == nil || e.Meta.Absent.Reason != ReasonSchemaMismatch {
			t.Fatalf("%s", out)
		}
	})
	t.Run("42703 dentro de tx -> error no degradable (la tx quedó abortada)", func(t *testing.T) {
		tx, mock, cleanup := newMockGormTx(t)
		defer cleanup()
		mock.ExpectQuery(`to_regclass`).WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow(false))
		expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnError(pgErr("42703", `column "uuid" does not exist`))
		inv := testInvocation(nil, nil, uuid.New(), fiscalReadEnforcer(), nil)
		inv.tx, inv.db = tx, tx
		e := decodeSU(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","where":{"uuid":"x"},"on_absent":"empty"}`)))
		if e.Success || e.Error.Code != CodeSourceUnavailable || e.Error.Details.Degradable || e.Error.Details.Reason != ReasonSchemaMismatch {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("42501 sigue db_error con el flag", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" LIMIT 0`).WillReturnError(pgErr("42501", "permission denied for table fiscal_documents"))
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		e := decodeSU(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","on_absent":"empty"}`)))
		if e.Success || e.Error.Code != "db_error" || e.Error.Details != nil {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("42P01 posterior al probe sigue db_error con el flag", func(t *testing.T) {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnError(pgErr("42P01", `relation "fiscal_documents_ext" does not exist`))
		inv := testInvocation(gdb, nil, uuid.New(), fiscalReadEnforcer(), nil)
		e := decodeSU(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","on_absent":"empty"}`)))
		if e.Success || e.Error.Code != "db_error" {
			t.Fatalf("%+v", e)
		}
	})
}

// Dentro de la tx del llamador la tabla base se comprueba con to_regclass (no
// aborta): ausente -> éxito vacío; presente -> lectura normal sobre la misma tx.
func TestOnAbsent_InTxUsesToRegclass(t *testing.T) {
	t.Run("ausente", func(t *testing.T) {
		tx, mock, cleanup := newMockGormTx(t)
		defer cleanup()
		mock.ExpectQuery(`SELECT to_regclass\(\$1\) IS NULL`).WithArgs(`"fiscal_documents"`).WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow(true))
		inv := testInvocation(nil, nil, uuid.New(), fiscalReadEnforcer(), nil)
		inv.tx, inv.db = tx, tx
		out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","on_absent":"empty"}`))
		if e := decodeAbsentOK(t, out); !e.Success || e.Meta.Absent == nil || e.Meta.Absent.Reason != ReasonTableMissing {
			t.Fatalf("%s", out)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("sin flag no hay to_regclass", func(t *testing.T) {
		tx, mock, cleanup := newMockGormTx(t)
		defer cleanup()
		expectProbe(mock, `"fiscal_documents"`, "id", "organization_id")
		mock.ExpectQuery(`SELECT \* FROM "fiscal_documents" WHERE`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
		inv := testInvocation(nil, nil, uuid.New(), fiscalReadEnforcer(), nil)
		inv.tx, inv.db = tx, tx
		if e := decodeAbsentOK(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents"}`))); !e.Success {
			t.Fatal("read must succeed")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("error de to_regclass es db_error", func(t *testing.T) {
		tx, mock, cleanup := newMockGormTx(t)
		defer cleanup()
		mock.ExpectQuery(`to_regclass`).WillReturnError(pgErr("57014", "canceling statement due to statement timeout"))
		inv := testInvocation(nil, nil, uuid.New(), fiscalReadEnforcer(), nil)
		inv.tx, inv.db = tx, tx
		e := decodeSU(t, executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"fiscal_documents","on_absent":"empty"}`)))
		if e.Success || e.Error.Code != "db_error" {
			t.Fatalf("%+v", e)
		}
	})
}

// data_mutate ignora on_absent (sigue source_unavailable no degradable).
func TestOnAbsent_DataMutateIgnoresIt(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnError(pgErr("42P01", `relation "stock" does not exist`))
	mock.ExpectRollback()
	inv := testInvocation(gdb, events.NewBus(nil), uuid.New(), stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","on_absent":"empty","data":{"quantity":1}}`))
	e := decodeSU(t, out)
	if e.Success || e.Error.Code != CodeSourceUnavailable || e.Error.Details.Degradable {
		t.Fatalf("%s", out)
	}
}
