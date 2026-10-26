package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DG1.1 against a REAL Postgres through the real pgx driver: the SQLSTATE the
// classifier reads is the one the driver produces, not one a mock invents. Needs
// TEST_POSTGRES_DSN (skips otherwise).

func openSourcePG(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sdb, _ := db.DB()
	t.Cleanup(func() { _ = sdb.Close() })
	return db
}

// oldGuestDatasetMissing is, VERBATIM, how published guests (addons `customers`
// returns_domain.go / `inventory` sale_policy.go) decide "the dataset is not
// there": a substring match of the SQLSTATE over the raw envelope, never the
// error code. It must keep working byte for byte against the new envelope.
func oldGuestDatasetMissing(rawEnvelope string) bool {
	return strings.Contains(rawEnvelope, "42P01")
}

// oldGuestErrorFields is how a published guest decodes a failed host envelope:
// a struct that reads only code and message (unknown keys are ignored).
func oldGuestErrorFields(t *testing.T, raw []byte) (code, message string) {
	t.Helper()
	var obj struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("old guest cannot decode the envelope: %v -- %s", err, raw)
	}
	return obj.Error.Code, obj.Error.Message
}

func TestSourceUnavailable_RealPostgres_ReadsAndOldGuestCompat(t *testing.T) {
	db := openSourcePG(t)
	orgID := uuid.New()
	missing := "dg1_missing_" + uuid.NewString()[:8]
	present := "dg1_present_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, amount int)`, present)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + present) })

	resolve := func(logical string) string {
		switch logical {
		case "fiscal_documents":
			return missing
		case "stock":
			return present
		}
		return logical
	}
	read := func(owners func(uuid.UUID, string) SourceInfo, payload string) []byte {
		inv := testInvocation(db, nil, orgID, fiscalAndStockReadEnforcer(), resolve)
		inv.sourceOwners = owners
		return executeDataQueryRecords(context.Background(), inv, []byte(payload))
	}

	t.Run("42P01 real -> source_unavailable addon_not_installed, texto intacto", func(t *testing.T) {
		out := read(func(uuid.UUID, string) SourceInfo { return SourceInfo{Owners: []string{"fiscal_mexico"}} },
			`{"table":"fiscal_documents"}`)
		e := decodeSU(t, out)
		if e.Error.Code != CodeSourceUnavailable || e.Error.Details == nil ||
			e.Error.Details.Reason != ReasonAddonNotInstalled || e.Error.Details.SQLState != "42P01" {
			t.Fatalf("real 42P01: %s", out)
		}
		if e.Error.LegacyCode != "db_error" {
			t.Fatalf("legacy_code: %s", out)
		}

		// Compatibility with a guest published BEFORE this change.
		if !oldGuestDatasetMissing(string(out)) {
			t.Fatalf("an old guest that greps 42P01 over the raw envelope stopped matching: %s", out)
		}
		code, msg := oldGuestErrorFields(t, out)
		if !strings.Contains(msg, "42P01") || !strings.Contains(msg, `relation "`+missing+`" does not exist`) {
			t.Fatalf("message must keep the driver text with SQLSTATE: %q (code %q)", msg, code)
		}
	})

	t.Run("sin resolver de dueños: table_missing", func(t *testing.T) {
		e := decodeSU(t, read(nil, `{"table":"fiscal_documents"}`))
		if e.Error.Code != CodeSourceUnavailable || e.Error.Details.Reason != ReasonTableMissing {
			t.Fatalf("no owners resolver: %+v", e.Error)
		}
	})

	t.Run("42703 real -> schema_mismatch, y un guest viejo NO lo toma por tabla ausente", func(t *testing.T) {
		out := read(nil, `{"table":"stock","where":{"no_such_col":"x"}}`)
		e := decodeSU(t, out)
		if e.Error.Code != CodeSourceUnavailable || e.Error.Details.Reason != ReasonSchemaMismatch || e.Error.Details.SQLState != "42703" {
			t.Fatalf("real 42703: %s", out)
		}
		if oldGuestDatasetMissing(string(out)) {
			t.Fatalf("a column mismatch must not look like a missing table to an old guest: %s", out)
		}
	})

	t.Run("tabla existente: sin cambios (éxito con filas vacías)", func(t *testing.T) {
		out := read(nil, `{"table":"stock"}`)
		env := unmarshalDataQuery(t, out)
		if !env.Success || env.Data == nil || len(env.Data.Rows) != 0 {
			t.Fatalf("existing table must behave exactly as before: %s", out)
		}
	})
}

func TestSourceUnavailable_RealPostgres_WriteToMissingTable(t *testing.T) {
	db := openSourcePG(t)
	missing := "dg1_missing_" + uuid.NewString()[:8]
	resolve := func(string) string { return missing }
	inv := testInvocation(db, events.NewBus(nil), uuid.New(), stockWriteEnforcer(), resolve)
	inv.sourceOwners = func(uuid.UUID, string) SourceInfo {
		return SourceInfo{Owners: []string{"inventory"}, Disabled: []string{"inventory"}}
	}
	out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}}`))
	e := decodeSU(t, out)
	if e.Error.Code != CodeSourceUnavailable || e.Error.Details.Reason != ReasonAddonDisabled || e.Error.Details.Degradable {
		t.Fatalf("write to absent table: %s", out)
	}
	if !oldGuestDatasetMissing(string(out)) {
		t.Fatalf("old guest compat on writes: %s", out)
	}

	batch := executeDataBatch(context.Background(), inv, []byte(`{"mutations":[{"op":"create","table":"stock","model":"Stock","data":{"quantity":1}}]}`))
	be := decodeSU(t, batch)
	if be.Error.Code != CodeSourceUnavailable || !strings.HasPrefix(be.Error.Message, "mutations[0]: ") {
		t.Fatalf("batch: %s", batch)
	}
}

// A non-absence failure (permission denied, 42501) is NOT an absence: it stays db_error
// so a money/stock guard keeps failing closed.
func TestSourceUnavailable_RealPostgres_PermissionDeniedStaysDBError(t *testing.T) {
	db := openSourcePG(t)
	table := "dg1_slow_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	// A permission failure is a deterministic non-absence SQLSTATE (42501).
	role := "dg1_ro_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, role)).Error; err != nil {
		t.Skipf("cannot create role: %v", err)
	}
	t.Cleanup(func() { db.Exec("DROP ROLE IF EXISTS " + role) })
	tx := db.Begin()
	defer tx.Rollback()
	if err := tx.Exec("SET LOCAL ROLE " + role).Error; err != nil {
		t.Skipf("cannot switch role: %v", err)
	}
	inv := testInvocation(tx, nil, uuid.New(), fiscalAndStockReadEnforcer(), func(string) string { return table })
	out := executeDataQueryRecords(context.Background(), inv, []byte(`{"table":"stock"}`))
	e := decodeSU(t, out)
	if e.Error.Code != "db_error" || !onlyClassified(e) {
		t.Fatalf("permission denied (42501) must stay db_error: %s", out)
	}
}

// ---- DG1.2: on_absent against a real Postgres -----------------------------

func pgQuery(db *gorm.DB, tx *gorm.DB, orgID uuid.UUID, owners func(uuid.UUID, string) SourceInfo, resolve func(string) string, payload string) []byte {
	inv := testInvocation(db, nil, orgID, fiscalAndStockReadEnforcer(), resolve)
	if tx != nil {
		inv.tx, inv.db = tx, tx
	}
	inv.sourceOwners = owners
	return executeDataQueryRecords(context.Background(), inv, []byte(payload))
}

// P3: the table EXISTS (org A installed the addon) but org B has no owner
// installed: with the flag B gets "absent" and never reads A's table; A reads its rows.
func TestOnAbsent_RealPostgres_MultiOrgInstallRule(t *testing.T) {
	db := openSourcePG(t)
	orgA, orgB := uuid.New(), uuid.New()
	table := "dg12_docs_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	if err := db.Exec(fmt.Sprintf(`INSERT INTO %s VALUES (?, ?)`, table), uuid.New(), orgA).Error; err != nil {
		t.Fatal(err)
	}
	owners := func(org uuid.UUID, _ string) SourceInfo {
		if org == orgA {
			return SourceInfo{Owners: []string{"fiscal_mexico"}, Installed: []string{"fiscal_mexico"}}
		}
		return SourceInfo{Owners: []string{"fiscal_mexico"}}
	}
	resolve := func(string) string { return table }
	const p = `{"table":"fiscal_documents","on_absent":"empty"}`

	a := decodeAbsentOK(t, pgQuery(db, nil, orgA, owners, resolve, p))
	if !a.Success || len(a.Data.Rows) != 1 || a.Meta.Absent != nil {
		t.Fatalf("org A must read its row: %+v", a)
	}
	bOut := pgQuery(db, nil, orgB, owners, resolve, p)
	b := decodeAbsentOK(t, bOut)
	if !b.Success || len(b.Data.Rows) != 0 || b.Meta.Absent == nil || b.Meta.Absent.Reason != ReasonAddonNotInstalled {
		t.Fatalf("org B must be absent: %s", bOut)
	}
	// Same org B without the flag: physical behaviour of today (success, 0 rows, no meta.absent).
	nb := decodeAbsentOK(t, pgQuery(db, nil, orgB, owners, resolve, `{"table":"fiscal_documents"}`))
	if !nb.Success || nb.Meta.Absent != nil {
		t.Fatalf("without the flag nothing changes: %+v", nb)
	}
}

func TestOnAbsent_RealPostgres_MissingTable(t *testing.T) {
	db := openSourcePG(t)
	missing := "dg12_missing_" + uuid.NewString()[:8]
	resolve := func(string) string { return missing }
	owners := func(uuid.UUID, string) SourceInfo {
		return SourceInfo{Owners: []string{"fiscal_mexico"}, Installed: []string{"fiscal_mexico"}}
	}

	t.Run("fuera de tx", func(t *testing.T) {
		for _, p := range []string{
			`{"table":"fiscal_documents","on_absent":"empty"}`,
			`{"table":"fiscal_documents","on_absent":"empty","aggregate":{"select":[{"fn":"count","as":"n"}]}}`,
		} {
			out := pgQuery(db, nil, uuid.New(), owners, resolve, p)
			e := decodeAbsentOK(t, out)
			if !e.Success || e.Meta.Absent == nil || e.Meta.Absent.Reason != ReasonTableMissing || rawData(t, out) != `{"rows":[],"truncated":false}` {
				t.Fatalf("%s -> %s", p, out)
			}
		}
	})

	t.Run("dentro de la tx del llamador la tx sigue usable (to_regclass)", func(t *testing.T) {
		tx := db.Begin()
		defer tx.Rollback()
		out := pgQuery(db, tx, uuid.New(), owners, resolve, `{"table":"fiscal_documents","on_absent":"empty"}`)
		if e := decodeAbsentOK(t, out); !e.Success || e.Meta.Absent == nil {
			t.Fatalf("%s", out)
		}
		var one int
		if err := tx.Raw("SELECT 1").Row().Scan(&one); err != nil || one != 1 {
			t.Fatalf("the caller's tx must NOT be aborted (25P02): %v", err)
		}
		// Without the flag the tx is aborted by the failed probe, as before.
		out = pgQuery(db, tx, uuid.New(), owners, resolve, `{"table":"fiscal_documents"}`)
		e := decodeSU(t, out)
		if e.Error.Code != CodeSourceUnavailable || e.Error.Details.Degradable {
			t.Fatalf("%s", out)
		}
	})

	t.Run("tabla con nombre de esquema citado", func(t *testing.T) {
		tx := db.Begin()
		defer tx.Rollback()
		out := pgQuery(db, tx, uuid.New(), owners, func(string) string { return "public." + missing }, `{"table":"fiscal_documents","on_absent":"empty"}`)
		if e := decodeAbsentOK(t, out); !e.Success || e.Meta.Absent == nil {
			t.Fatalf("%s", out)
		}
	})

	t.Run("data_mutate ignora on_absent", func(t *testing.T) {
		inv := testInvocation(db, events.NewBus(nil), uuid.New(), stockWriteEnforcer(), resolve)
		out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","on_absent":"empty","data":{"quantity":1}}`))
		if e := decodeSU(t, out); e.Success || e.Error.Code != CodeSourceUnavailable || e.Error.Details.Degradable {
			t.Fatalf("%s", out)
		}
	})
}

func TestOnAbsent_RealPostgres_UndefinedColumn(t *testing.T) {
	db := openSourcePG(t)
	table := "dg12_cols_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid)`, table)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	resolve := func(string) string { return table }
	const p = `{"table":"stock","where":{"no_such_col":"x"},"on_absent":"empty"}`

	out := pgQuery(db, nil, uuid.New(), nil, resolve, p)
	if e := decodeAbsentOK(t, out); !e.Success || e.Meta.Absent == nil || e.Meta.Absent.Reason != ReasonSchemaMismatch {
		t.Fatalf("outside a tx 42703 degrades: %s", out)
	}
	tx := db.Begin()
	defer tx.Rollback()
	out = pgQuery(db, tx, uuid.New(), nil, resolve, p)
	e := decodeSU(t, out)
	if e.Success || e.Error.Code != CodeSourceUnavailable || e.Error.Details.Degradable || e.Error.Details.SQLState != "42703" {
		t.Fatalf("inside a tx 42703 is NOT degradable: %s", out)
	}
}

// Timeout (57014) and permission (42501) are not absences: db_error even with the flag.
func TestOnAbsent_RealPostgres_TimeoutAndPermissionDoNotDegrade(t *testing.T) {
	db := openSourcePG(t)
	slow := "dg12_slow_" + uuid.NewString()[:8]
	if err := db.Exec(fmt.Sprintf(`CREATE VIEW %s AS SELECT gen_random_uuid() AS id, gen_random_uuid() AS organization_id, (pg_sleep(1) IS NULL) AS s`, slow)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP VIEW IF EXISTS " + slow) })
	t.Run("timeout", func(t *testing.T) {
		tx := db.Begin()
		defer tx.Rollback()
		if err := tx.Exec("SET LOCAL statement_timeout = '50ms'").Error; err != nil {
			t.Fatal(err)
		}
		out := pgQuery(db, tx, uuid.New(), nil, func(string) string { return slow }, `{"table":"stock","on_absent":"empty"}`)
		e := decodeSU(t, out)
		if e.Success || e.Error.Code != "db_error" || !onlyClassified(e) {
			t.Fatalf("57014 must stay db_error: %s", out)
		}
	})
	t.Run("permiso", func(t *testing.T) {
		table := "dg12_perm_" + uuid.NewString()[:8]
		if err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid)`, table)).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
		role := "dg12_ro_" + uuid.NewString()[:8]
		if err := db.Exec(fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, role)).Error; err != nil {
			t.Skipf("cannot create role: %v", err)
		}
		t.Cleanup(func() { db.Exec("DROP ROLE IF EXISTS " + role) })
		tx := db.Begin()
		defer tx.Rollback()
		if err := tx.Exec("SET LOCAL ROLE " + role).Error; err != nil {
			t.Skipf("cannot switch role: %v", err)
		}
		// Role has no privileges on the table (it can still resolve it): in a tx
		// to_regclass is not NULL, the probe then fails with 42501.
		out := pgQuery(db, tx, uuid.New(), nil, func(string) string { return table }, `{"table":"stock","on_absent":"empty"}`)
		e := decodeSU(t, out)
		if e.Success || e.Error.Code != "db_error" || !onlyClassified(e) {
			t.Fatalf("42501 must stay db_error: %s", out)
		}
	})
}
