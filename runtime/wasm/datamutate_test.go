package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// dmEnvelope is the data_mutate wire shape (docs/wasm-abi.md § 14.5).
type dmEnvelope struct {
	Success bool           `json:"success"`
	Data    *dmData        `json:"data,omitempty"`
	Error   *dbqError      `json:"error,omitempty"`
	Meta    map[string]any `json:"meta"`
}

type dmData struct {
	ID     string         `json:"id"`
	Before map[string]any `json:"before"`
	After  map[string]any `json:"after"`
}

func unmarshalMutate(t *testing.T, raw []byte) dmEnvelope {
	t.Helper()
	var env dmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v -- %s", err, raw)
	}
	return env
}

// stockWriteEnforcer grants `db:write stock` (the logical table the tests
// mutate) and nothing else — the shape a real manifest declaration compiles
// to. The implicit addon_<key>.* grant does NOT cover logical tables, so a
// declared capability is required, exactly like production.
func stockWriteEnforcer() *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{
			{Kind: "db:write", Target: "stock"},
		})
	})
	e.SetMode(security.ModeEnforce)
	return e
}

// captureBus returns a Bus plus an accessor for the canonical events a
// data_mutate publish delivered. Subscriptions register as "kernel" so no
// event:subscribe capability is involved.
func captureBus(t *testing.T, pattern string) (*events.Bus, func() []*dynamic.CanonicalEvent, func() []uuid.UUID) {
	t.Helper()
	bus := events.NewBus(nil)
	var mu sync.Mutex
	var got []*dynamic.CanonicalEvent
	var orgs []uuid.UUID
	err := bus.Subscribe("kernel", pattern, func(_ context.Context, orgID uuid.UUID, payload any) error {
		ce, ok := payload.(*dynamic.CanonicalEvent)
		if !ok {
			t.Errorf("payload is %T, want *dynamic.CanonicalEvent", payload)
			return nil
		}
		mu.Lock()
		got = append(got, ce)
		orgs = append(orgs, orgID)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return bus,
		func() []*dynamic.CanonicalEvent { mu.Lock(); defer mu.Unlock(); return got },
		func() []uuid.UUID { mu.Lock(); defer mu.Unlock(); return orgs }
}

func testInvocation(db *gorm.DB, bus *events.Bus, orgID uuid.UUID, enforcer *security.Enforcer, resolve func(string) string) *invocation {
	return &invocation{
		addonKey:     "inventory",
		orgID:        orgID,
		db:           db,
		bus:          bus,
		enforcer:     enforcer,
		resolveTable: resolve,
	}
}

func TestExecuteDataMutate_CreateStampsOrgIDTimestamps(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, getEvents, getOrgs := captureBus(t, "inventory.Stock.created")

	// Columns are sorted alphabetically and the host stamps id /
	// organization_id / created_at / updated_at — the INSERT shape proves it.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "stock" \("created_at", "id", "organization_id", "product_id", "quantity", "updated_at"\) VALUES \(\$1, \$2, \$3, \$4, \$5, \$6\) RETURNING \*`).
		WithArgs(sqlmock.AnyArg(), rowID, orgID, "prod-1", int64(5), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "product_id", "quantity"}).
			AddRow(rowID, orgID.String(), "prod-1", int64(5)))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	ctx := dynamic.WithCorrelationID(context.Background(), "corr-123")
	out := executeDataMutate(ctx, inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"id": "`+rowID+`",
		"data": {"product_id": "prod-1", "quantity": 5}
	}`))

	env := unmarshalMutate(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data == nil || env.Data.ID != rowID {
		t.Fatalf("expected id %s, got %#v", rowID, env.Data)
	}
	if env.Data.Before != nil {
		t.Fatalf("create must have nil before, got %#v", env.Data.Before)
	}
	if env.Data.After == nil || env.Data.After["product_id"] != "prod-1" {
		t.Fatalf("expected after row, got %#v", env.Data.After)
	}
	if env.Meta["orgId"] != orgID.String() {
		t.Fatalf("expected meta.orgId %s, got %v", orgID, env.Meta["orgId"])
	}
	if env.Meta["envelopeVersion"] != float64(1) {
		t.Fatalf("expected envelopeVersion 1, got %v", env.Meta["envelopeVersion"])
	}

	evs := getEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 canonical event, got %d", len(evs))
	}
	ce := evs[0]
	if ce.Action != "created" || ce.Model != "Stock" || ce.AddonKey != "inventory" {
		t.Fatalf("unexpected event envelope %#v", ce)
	}
	if ce.ID != rowID || ce.Before != nil || ce.After == nil {
		t.Fatalf("unexpected event rows %#v", ce)
	}
	if ce.CorrelationID != "corr-123" {
		t.Fatalf("expected correlation corr-123, got %q", ce.CorrelationID)
	}
	if orgs := getOrgs(); len(orgs) != 1 || orgs[0] != orgID {
		t.Fatalf("event delivered with wrong org %v", orgs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_CreateStampsCreatedByFromActor(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	actorID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.created")

	mock.ExpectBegin()
	// The actor in ctx triggers the zero-row column probe; the table carries
	// created_by_id, so the INSERT stamps it (sorted into the column list).
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "created_by_id", "product_id", "quantity"}))
	mock.ExpectQuery(`INSERT INTO "stock" \("created_at", "created_by_id", "id", "organization_id", "product_id", "quantity", "updated_at"\) VALUES \(\$1, \$2, \$3, \$4, \$5, \$6, \$7\) RETURNING \*`).
		WithArgs(sqlmock.AnyArg(), actorID, rowID, orgID, "prod-1", int64(5), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_by_id", "product_id"}).
			AddRow(rowID, actorID, "prod-1"))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	ctx := dynamic.WithActorID(context.Background(), actorID)
	out := executeDataMutate(ctx, inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"id": "`+rowID+`",
		"data": {"product_id": "prod-1", "quantity": 5}
	}`))

	env := unmarshalMutate(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data.After["created_by_id"] != actorID {
		t.Fatalf("expected created_by_id %s, got %#v", actorID, env.Data.After)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_CreateSkipsCreatedByWithoutColumn(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	actorID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.created")

	mock.ExpectBegin()
	// Probe says the table has NO created_by_id column → the INSERT keeps the
	// original shape, no phantom column.
	mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "product_id", "quantity"}))
	mock.ExpectQuery(`INSERT INTO "stock" \("created_at", "id", "organization_id", "product_id", "quantity", "updated_at"\) VALUES \(\$1, \$2, \$3, \$4, \$5, \$6\) RETURNING \*`).
		WithArgs(sqlmock.AnyArg(), rowID, orgID, "prod-1", int64(5), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "product_id"}).AddRow(rowID, "prod-1"))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	ctx := dynamic.WithActorID(context.Background(), actorID)
	out := executeDataMutate(ctx, inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"id": "`+rowID+`",
		"data": {"product_id": "prod-1", "quantity": 5}
	}`))

	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_UpdateIncAtomicBeforeAfter(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.updated")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "quantity", "note"}).
			AddRow(rowID, int64(10), "old"))
	// data SETs sorted first, then inc as `col = col + $n` (atomic — never a
	// read-modify-write on the snapshot), then the updated_at stamp.
	mock.ExpectQuery(`UPDATE "stock" SET "note" = \$1, "quantity" = "quantity" \+ \$2, "updated_at" = \$3 WHERE id = \$4 AND organization_id = \$5 RETURNING \*`).
		WithArgs("new", int64(-3), sqlmock.AnyArg(), rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "quantity", "note"}).
			AddRow(rowID, int64(7), "new"))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "update", "table": "stock", "model": "Stock",
		"id": "`+rowID+`",
		"data": {"note": "new"},
		"inc": {"quantity": -3}
	}`))

	env := unmarshalMutate(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data.Before == nil || env.Data.Before["quantity"] != float64(10) {
		t.Fatalf("expected before.quantity=10, got %#v", env.Data.Before)
	}
	if env.Data.After == nil || env.Data.After["quantity"] != float64(7) {
		t.Fatalf("expected after.quantity=7, got %#v", env.Data.After)
	}

	evs := getEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 canonical event, got %d", len(evs))
	}
	if evs[0].Action != "updated" || evs[0].Before == nil || evs[0].After == nil {
		t.Fatalf("unexpected event %#v", evs[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_DeleteSoftWhenDeletedAtPresent(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.deleted")

	mock.ExpectBegin()
	// Snapshot carries a deleted_at column (NULL) → soft-delete path.
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "quantity", "deleted_at"}).
			AddRow(rowID, int64(10), nil))
	mock.ExpectExec(`UPDATE "stock" SET "deleted_at" = \$1, "updated_at" = \$2 WHERE id = \$3 AND organization_id = \$4`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), rowID, orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "delete", "table": "stock", "model": "Stock", "id": "`+rowID+`"
	}`))

	env := unmarshalMutate(t, out)
	if !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if env.Data.Before == nil || env.Data.After != nil {
		t.Fatalf("delete must carry before and nil after, got %#v", env.Data)
	}
	evs := getEvents()
	if len(evs) != 1 || evs[0].Action != "deleted" || evs[0].After != nil || evs[0].Before == nil {
		t.Fatalf("unexpected canonical event %#v", evs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_DeleteHardWithoutDeletedAt(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.deleted")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "quantity"}).
			AddRow(rowID, int64(10)))
	mock.ExpectExec(`DELETE FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "delete", "table": "stock", "model": "Stock", "id": "`+rowID+`"
	}`))

	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_OrgEnforcement(t *testing.T) {
	// A row owned by another org is invisible: the org-scoped snapshot SELECT
	// matches nothing and the import answers not_found WITHOUT running the
	// UPDATE. The org id comes from the invocation, never the request.
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, getEvents, _ := captureBus(t, "inventory.Stock.*")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "update", "table": "stock", "model": "Stock",
		"id": "`+rowID+`", "data": {"note": "x"}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("expected not_found, got %s", out)
	}
	if len(getEvents()) != 0 {
		t.Fatal("no event must be published on not_found")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_GuestSuppliedOrgIDRejected(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	bus, _, _ := captureBus(t, "inventory.Stock.created")
	inv := testInvocation(gdb, bus, uuid.New(), stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"data": {"organization_id": "`+uuid.NewString()+`"}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Fatalf("expected invalid_request for guest organization_id, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDataMutate_CapabilityDenied(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	// The implicit addon_<key>.* grant does NOT cover the logical table —
	// without a declared `db:write stock` the import answers forbidden before
	// touching the driver.
	bus, getEvents, _ := captureBus(t, "inventory.Stock.created")
	inv := testInvocation(gdb, bus, uuid.New(), permissiveEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"data": {"quantity": 1}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("expected forbidden, got %s", out)
	}
	if !strings.Contains(env.Error.Message, "db:write") {
		t.Fatalf("expected message to mention db:write, got %q", env.Error.Message)
	}
	if len(getEvents()) != 0 {
		t.Fatal("no event must be published on forbidden")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDataMutate_NotFoundOnDelete(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.deleted")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "stock" WHERE id = \$1 AND organization_id = \$2`).
		WithArgs(rowID, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()

	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "delete", "table": "stock", "model": "Stock", "id": "`+rowID+`"
	}`))

	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("expected not_found, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_TableResolverQualifiesPhysicalName(t *testing.T) {
	// The capability check runs on the LOGICAL name; the SQL runs on the
	// resolver-produced physical name (here public.stock — the ops mapping).
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	orgID := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.Stock.created")

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "public"\."stock" \(.+\) VALUES \(.+\) RETURNING \*`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(rowID))
	mock.ExpectCommit()

	resolve := func(table string) string { return "public." + table }
	inv := testInvocation(gdb, bus, orgID, stockWriteEnforcer(), resolve)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"id": "`+rowID+`", "data": {"quantity": 1}
	}`))

	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestExecuteDataMutate_BusUnavailable(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	inv := testInvocation(gdb, nil, uuid.New(), stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock", "data": {"quantity": 1}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "bus_unavailable" {
		t.Fatalf("expected bus_unavailable, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDataMutate_NoActiveOrg(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()

	bus, _, _ := captureBus(t, "inventory.Stock.created")
	inv := testInvocation(gdb, bus, uuid.Nil, stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock", "data": {"quantity": 1}
	}`))

	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Fatalf("expected invalid_request for missing org, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("driver should be untouched: %v", err)
	}
}

func TestExecuteDataMutate_ValidateRequest(t *testing.T) {
	bad := []string{
		`{"op": "upsert", "table": "stock", "model": "Stock"}`,                                       // bad op
		`{"op": "create", "table": "public.stock", "model": "Stock"}`,                                // qualified table
		`{"op": "create", "table": "stock; DROP", "model": "Stock"}`,                                 // injection shape
		`{"op": "create", "table": "stock"}`,                                                         // missing model
		`{"op": "update", "table": "stock", "model": "Stock", "data": {"a": 1}}`,                     // missing id
		`{"op": "update", "table": "stock", "model": "Stock", "id": "not-a-uuid", "data": {"a": 1}}`, // bad id
		`{"op": "delete", "table": "stock", "model": "Stock"}`,                                       // missing id
		`{"op": "create", "table": "stock", "model": "Stock", "inc": {"a": 1}}`,                      // inc on create
		`{"op": "create", "table": "stock", "model": "Stock", "data": {"created_at": "x"}}`,          // reserved col
		`{"op": "create", "table": "stock", "model": "Stock", "data": {"a;b": 1}}`,                   // bad column
	}
	id := uuid.NewString()
	bad = append(bad,
		`{"op": "update", "table": "stock", "model": "Stock", "id": "`+id+`"}`,                                    // no data/inc
		`{"op": "update", "table": "stock", "model": "Stock", "id": "`+id+`", "data": {"q": 1}, "inc": {"q": 1}}`, // overlap
		`{"op": "update", "table": "stock", "model": "Stock", "id": "`+id+`", "inc": {"q": "x"}}`,                 // non-numeric inc
	)
	for _, req := range bad {
		bus, _, _ := captureBus(t, "inventory.Stock.*")
		inv := testInvocation(nil, bus, uuid.New(), nil, nil)
		out := executeDataMutate(context.Background(), inv, []byte(req))
		env := unmarshalMutate(t, out)
		if env.Success || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Errorf("expected invalid_request for %s, got %s", req, out)
		}
	}
}
