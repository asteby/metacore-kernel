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
	if e.Error.Code != "db_error" || e.Error.Details != nil {
		t.Fatalf("permission denied (42501) must stay db_error: %s", out)
	}
}
