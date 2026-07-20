package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// dbatchEnvelope is the data_batch wire shape (docs/wasm-abi.md § 16).
type dbatchEnvelope struct {
	Success bool           `json:"success"`
	Data    *dbatchData    `json:"data,omitempty"`
	Error   *dbqError      `json:"error,omitempty"`
	Meta    map[string]any `json:"meta"`
}

type dbatchData struct {
	Count   int            `json:"count"`
	Results []dataBatchRow `json:"results"`
}

func unmarshalBatch(t *testing.T, raw []byte) dbatchEnvelope {
	t.Helper()
	var env dbatchEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v -- %s", err, raw)
	}
	return env
}

// multiWriteEnforcer grants db:write on every table named — the shape a real
// multi-model batch declaration compiles to.
func multiWriteEnforcer(tables ...string) *security.Enforcer {
	caps := make([]manifest.Capability, len(tables))
	for i, tbl := range tables {
		caps[i] = manifest.Capability{Kind: "db:write", Target: tbl}
	}
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, caps)
	})
	e.SetMode(security.ModeEnforce)
	return e
}

func TestExecuteDataBatch_AtomicMultiModelCommit(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	stockID := uuid.NewString()
	ledgerID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.*")

	mock.ExpectBegin()
	// Mutation 0: update loads the row snapshot first, then decrements quantity.
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(stockID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(stockID, orgID.String(), int64(10)))
	mock.ExpectQuery(`UPDATE "stock" SET "quantity" = "quantity" \+ \$1, "updated_at" = \$2 WHERE id = \$3 AND organization_id = \$4 RETURNING \*`).
		WithArgs(int64(-3), sqlmock.AnyArg(), stockID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).
			AddRow(stockID, orgID.String(), int64(7)))
	// Mutation 1: append the ledger row.
	mock.ExpectQuery(`SELECT \* FROM "ledger" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`INSERT INTO "ledger" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "delta"}).
			AddRow(ledgerID, orgID.String(), int64(-3)))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, multiWriteEnforcer("stock", "ledger"), nil)
	ctx := dynamic.WithCorrelationID(context.Background(), "corr-batch")
	out := executeDataBatch(ctx, inv, []byte(fmt.Sprintf(`{"mutations":[
		{"op":"update","table":"stock","model":"Stock","id":%q,"inc":{"quantity":-3}},
		{"op":"create","table":"ledger","model":"Ledger","id":%q,"data":{"delta":-3}}
	]}`, stockID, ledgerID)))

	env := unmarshalBatch(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data.Count != 2 || len(env.Data.Results) != 2 {
		t.Fatalf("want 2 results, got %+v", env.Data)
	}
	if env.Data.Results[0].Action != "updated" || env.Data.Results[1].Action != "created" {
		t.Fatalf("unexpected actions: %+v", env.Data.Results)
	}
	if got := getEvents(); len(got) != 2 {
		t.Fatalf("want 2 canonical events, got %d", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestExecuteDataBatch_RollsBackOnRowFailure(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	stockID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.*")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`INSERT INTO "stock" .* RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}).AddRow(stockID, orgID.String()))
	// Second mutation targets a missing row → not_found → whole batch rolls back.
	mock.ExpectQuery(`SELECT \* FROM "ledger" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
	mock.ExpectQuery(`SELECT \* FROM "ledger" WHERE id = \$1 AND organization_id = \$2`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()

	missing := uuid.NewString()
	inv := testInvocation(gdb, bus, orgID, multiWriteEnforcer("stock", "ledger"), nil)
	out := executeDataBatch(context.Background(), inv, []byte(fmt.Sprintf(`{"mutations":[
		{"op":"create","table":"stock","model":"Stock","id":%q,"data":{"delta":1}},
		{"op":"update","table":"ledger","model":"Ledger","id":%q,"data":{"delta":2}}
	]}`, stockID, missing)))

	env := unmarshalBatch(t, out)
	if env.Success {
		t.Fatalf("expected failure, got %s", out)
	}
	if env.Error.Code != "not_found" {
		t.Fatalf("want not_found, got %q", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "mutations[1]") {
		t.Fatalf("error should name the failing index: %q", env.Error.Message)
	}
	if got := getEvents(); len(got) != 0 {
		t.Fatalf("rolled-back batch must publish NO events, got %d", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestExecuteDataBatch_CapabilityDeniedForOneTable(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	bus, _, _ := captureBus(t, "inventory.*")
	// Grant stock but NOT ledger — the batch must be rejected before any DB work.
	inv := testInvocation(gdb, bus, orgID, multiWriteEnforcer("stock"), nil)
	out := executeDataBatch(context.Background(), inv, []byte(`{"mutations":[
		{"op":"create","table":"stock","model":"Stock","data":{"delta":1}},
		{"op":"create","table":"ledger","model":"Ledger","data":{"delta":2}}
	]}`))

	env := unmarshalBatch(t, out)
	if env.Success || env.Error.Code != "forbidden" {
		t.Fatalf("want forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "mutations[1]") {
		t.Fatalf("error should name the denied index: %q", env.Error.Message)
	}
}

func TestExecuteDataBatch_RejectsEmptyAndOverCap(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()
	orgID := uuid.New()
	bus, _, _ := captureBus(t, "inventory.*")
	inv := testInvocation(gdb, bus, orgID, multiWriteEnforcer("stock"), nil)

	empty := unmarshalBatch(t, executeDataBatch(context.Background(), inv, []byte(`{"mutations":[]}`)))
	if empty.Success || empty.Error.Code != "invalid_request" {
		t.Fatalf("empty batch should be invalid_request, got %+v", empty.Error)
	}

	var sb strings.Builder
	sb.WriteString(`{"mutations":[`)
	for i := 0; i < dataBatchMaxMutations+1; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"op":"create","table":"stock","model":"Stock","data":{"delta":1}}`)
	}
	sb.WriteString(`]}`)
	over := unmarshalBatch(t, executeDataBatch(context.Background(), inv, []byte(sb.String())))
	if over.Success || over.Error.Code != "invalid_request" {
		t.Fatalf("over-cap batch should be invalid_request, got %+v", over.Error)
	}
}
