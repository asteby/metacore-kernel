package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/runtime/wasm"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// ledgerDB returns an in-memory sqlite gorm.DB for the dispatch delivery ledger.
func ledgerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("ledger db: %v", err)
	}
	return db
}

// wasmDB returns a sqlmock-backed postgres gorm.DB the wasm db_exec import
// runs against, plus the mock to set expectations on. The dispatcher delivers
// asynchronously but WaitIdle serialises our assertions, and a single
// subscription means only one goroutine ever touches the mock at a time.
func wasmDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	mock.MatchExpectationsInOrder(false)
	gdb, err := gorm.Open(postgres.New(postgres.Config{
		Conn:                 sqlDB,
		PreferSimpleProtocol: true,
	}), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return gdb, mock
}

// permissiveEnforcer grants each addon its own implicit capabilities (db:write
// on its own schema, event:emit/subscribe per its compiled manifest). We feed
// it a manifest declaring event:subscribe + event:emit so the fixture addon is
// allowed to both receive the dispatched event and emit a secondary one.
func enforcerFor(addonKey string, caps []manifest.Capability) *security.Enforcer {
	e := security.NewEnforcer(func(k string) *security.Capabilities {
		if k == addonKey {
			return security.Compile(k, caps)
		}
		return security.Compile(k, nil)
	})
	e.SetMode(security.ModeEnforce)
	return e
}

const fixtureAddon = "stocker"

// fixtureCaps is the capability set the fixture addon declares: it subscribes
// to inventory events, emits a secondary one, and writes its own schema.
func fixtureCaps() []manifest.Capability {
	return []manifest.Capability{
		{Kind: "event:subscribe", Target: "inventory.*"},
		{Kind: "event:emit", Target: "stocker.recomputed"},
	}
}

// loadFixture compiles the db_exec+event_emit fixture wasm into a host and
// returns the host ready to InvokeFor("on_event").
func loadFixture(t *testing.T, ctx context.Context, bus *events.Bus, db *gorm.DB, enf *security.Enforcer) *wasm.Host {
	t.Helper()
	h, err := wasm.NewHost(ctx, security.Compile(fixtureAddon, fixtureCaps()), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	h.WithBus(bus).WithDB(db).WithEnforcer(enf)
	t.Cleanup(func() { _ = h.Close(ctx) })

	spec := &manifest.BackendSpec{
		Runtime:   "wasm",
		Entry:     "backend.wasm",
		Exports:   []string{"on_event"},
		TimeoutMs: 2000,
	}
	if err := h.Load(ctx, fixtureAddon, dbExecEmitWasm(), spec); err != nil {
		t.Fatalf("Load fixture: %v", err)
	}
	return h
}

// ---------------------------------------------------------------------------
// E2E: canonical event -> dispatcher -> wasm fixture mutates table + emits
//      secondary event; delivery ledger row = delivered.
// ---------------------------------------------------------------------------

func TestDispatch_E2E_WasmDelivery(t *testing.T) {
	ctx := context.Background()
	enf := enforcerFor(fixtureAddon, fixtureCaps())

	// Bus the dispatcher subscribes to AND the fixture's event_emit publishes
	// back onto. We attach the enforcer so the fixture's secondary emit is
	// capability-checked exactly as in production.
	bus := events.NewBus(enf)

	// Subscriber that proves the fixture's SECONDARY event ("stocker.recomputed")
	// actually fired.
	var secondary int32
	if err := bus.Subscribe("kernel", "stocker.recomputed", func(_ context.Context, _ uuid.UUID, _ any) error {
		atomic.AddInt32(&secondary, 1)
		return nil
	}); err != nil {
		t.Fatalf("subscribe secondary: %v", err)
	}

	wdb, mock := wasmDB(t)
	// The fixture's db_exec runs `UPDATE stock SET qty = qty - 1 WHERE id = $1`
	// on the standalone path: the host wraps it in its own Begin/Commit and
	// SET LOCAL search_path scoping. Assert the full sequence.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path TO "addon_stocker", public`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE stock SET qty = qty - 1`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	host := loadFixture(t, ctx, bus, wdb, enf)

	ldb := ledgerDB(t)
	installation := uuid.New()
	provider := dispatch.SubscriptionProviderFunc(func(_ context.Context, _ uuid.UUID) ([]dispatch.Subscription, error) {
		return []dispatch.Subscription{{
			AddonKey:     fixtureAddon,
			Installation: installation,
			Event:        "inventory.*",
			HandlerType:  "wasm",
			Function:     "on_event",
		}}, nil
	})

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, host, ldb, provider,
		dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enf)),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()),
	)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	// Publish a synthetic canonical CRUD event under the "inventory" addon.
	orgID := uuid.New()
	publishCanonical(t, bus, "inventory", "product", "updated", orgID, "row-1")

	awaitDeliveries(t, barrier, 1)

	// 1) the wasm fixture mutated the table (sqlmock expectations satisfied).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db_exec mutation not observed: %v", err)
	}
	// 2) the secondary event fired.
	if got := atomic.LoadInt32(&secondary); got != 1 {
		t.Fatalf("secondary event fired %d times, want 1", got)
	}
	// 3) the ledger row is delivered.
	got := singleDelivery(t, ldb)
	if got.Status != dispatch.StatusDelivered {
		t.Fatalf("delivery status = %q, want delivered (err=%q)", got.Status, got.LastError)
	}
	if got.AddonKey != fixtureAddon || got.Export != "on_event" {
		t.Fatalf("delivery row mis-recorded: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Idempotency: re-publishing the SAME event delivers exactly once.
// ---------------------------------------------------------------------------

func TestDispatch_Idempotent_Redelivery(t *testing.T) {
	ctx := context.Background()
	enf := enforcerFor(fixtureAddon, fixtureCaps())
	bus := events.NewBus(enf)

	wdb, mock := wasmDB(t)
	// Only ONE mutation (one Begin/Exec/Exec/Commit) is expected even though we
	// publish twice — the second publish is an idempotent ledger no-op.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL search_path`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE stock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	host := loadFixture(t, ctx, bus, wdb, enf)
	ldb := ledgerDB(t)
	provider := staticProvider(fixtureAddon, uuid.New(), "inventory.*", "wasm", "on_event")

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, host, ldb, provider,
		dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enf)),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	orgID := uuid.New()
	// Same (event-name, row-id) twice → same deterministic delivery_id.
	publishCanonical(t, bus, "inventory", "product", "updated", orgID, "row-1")
	publishCanonical(t, bus, "inventory", "product", "updated", orgID, "row-1")
	// Only the FIRST publish creates a delivery; the second is a ledger no-op
	// that never reaches a terminal callback. Await exactly one.
	awaitDeliveries(t, barrier, 1)
	// settle to let any (erroneous) second delivery surface.
	awaitDeliveries(t, barrier, 0)

	// Exactly one mutation despite two publishes.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("idempotency violated, extra mutation: %v", err)
	}
	if n := deliveryCount(t, ldb); n != 1 {
		t.Fatalf("ledger rows = %d, want 1 (idempotent)", n)
	}
}

// ---------------------------------------------------------------------------
// Failure -> retry -> dead-letter.
// ---------------------------------------------------------------------------

func TestDispatch_Failure_RetriesThenDead(t *testing.T) {
	bus := events.NewBus(nil)
	ldb := ledgerDB(t)

	var calls int32
	failing := dispatch.WasmInvokerFunc(func(_ context.Context, _, _ uuid.UUID, _, _ string, _ []byte, _ map[string]string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("boom")
	})

	provider := staticProvider("flaky", uuid.New(), "inventory.*", "wasm", "on_event")

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, failing, ldb, provider,
		dispatch.WithMaxAttempts(3),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	publishCanonical(t, bus, "inventory", "product", "updated", uuid.New(), "row-9")
	awaitDeliveries(t, barrier, 1)

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("invoker called %d times, want 3 (maxAttempts)", got)
	}
	row := singleDelivery(t, ldb)
	if row.Status != dispatch.StatusDead {
		t.Fatalf("status = %q, want dead", row.Status)
	}
	if row.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", row.Attempts)
	}
	if row.LastError == "" {
		t.Fatalf("dead delivery missing last_error")
	}
}

// ---------------------------------------------------------------------------
// Org without the addon enabled -> no delivery.
// ---------------------------------------------------------------------------

func TestDispatch_OrgWithoutAddon_NoDelivery(t *testing.T) {
	bus := events.NewBus(nil)
	ldb := ledgerDB(t)

	var calls int32
	invoker := dispatch.WasmInvokerFunc(func(_ context.Context, _, _ uuid.UUID, _, _ string, _ []byte, _ map[string]string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	})

	// Provider returns NO subscriptions for this org (addon not enabled here).
	provider := dispatch.SubscriptionProviderFunc(func(_ context.Context, _ uuid.UUID) ([]dispatch.Subscription, error) {
		return nil, nil
	})

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, invoker, ldb, provider,
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	publishCanonical(t, bus, "inventory", "product", "updated", uuid.New(), "row-x")
	awaitDeliveries(t, barrier, 0)

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("invoker called %d times, want 0 (addon not enabled for org)", got)
	}
	if n := deliveryCount(t, ldb); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Compiled tier routes to the in-process registry.
// ---------------------------------------------------------------------------

func TestDispatch_CompiledTier(t *testing.T) {
	bus := events.NewBus(nil)
	ldb := ledgerDB(t)

	var got atomic.Value // []byte
	reg := dispatch.MapCompiledRegistry{
		"recompute": dispatch.CompiledHandlerFunc(func(_ context.Context, _ uuid.UUID, payload []byte) error {
			got.Store(payload)
			return nil
		}),
	}
	provider := staticProvider("firstparty", uuid.New(), "inventory.product.updated", "compiled", "recompute")

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, nil, ldb, provider,
		dispatch.WithCompiledRegistry(reg),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	publishCanonical(t, bus, "inventory", "product", "updated", uuid.New(), "row-c")
	awaitDeliveries(t, barrier, 1)

	raw, _ := got.Load().([]byte)
	if raw == nil {
		t.Fatalf("compiled handler never invoked")
	}
	var ce map[string]any
	if err := json.Unmarshal(raw, &ce); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if ce["model"] != "product" || ce["action"] != "updated" {
		t.Fatalf("compiled handler got wrong payload: %v", ce)
	}
	if singleDelivery(t, ldb).Status != dispatch.StatusDelivered {
		t.Fatalf("compiled delivery not marked delivered")
	}
}

// awaitDeliveries blocks until n terminal deliveries have been observed on the
// barrier channel, or fails the test on timeout. n==0 means "expect none" — it
// just settles briefly to give any erroneous delivery a chance to surface.
func awaitDeliveries(t *testing.T, barrier <-chan dispatch.DeliveryResult, n int) {
	t.Helper()
	if n == 0 {
		select {
		case r := <-barrier:
			t.Fatalf("expected no delivery, got %+v", r)
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
	for i := 0; i < n; i++ {
		select {
		case <-barrier:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delivery %d/%d", i+1, n)
		}
	}
}
