package dynamic

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// ---------------------------------------------------------------------------
// Fake model
// ---------------------------------------------------------------------------

type TestProduct struct {
	modelbase.BaseUUIDModel
	Name  string  `json:"name" gorm:"size:255"`
	Price float64 `json:"price"`
}

func (TestProduct) TableName() string { return "test_products" }
func (TestProduct) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Test Products",
		Columns: []modelbase.ColumnDef{
			{Key: "name", Label: "Name", Sortable: true},
			{Key: "price", Label: "Price", Sortable: true},
		},
		SearchColumns: []string{"name"},
	}
}
func (TestProduct) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Test Product"}
}

// ---------------------------------------------------------------------------
// Fake AuthUser
// ---------------------------------------------------------------------------

type fakeUser struct {
	id    uuid.UUID
	orgID uuid.UUID
	role  string
}

func (u *fakeUser) GetID() uuid.UUID             { return u.id }
func (u *fakeUser) GetOrganizationID() uuid.UUID  { return u.orgID }
func (u *fakeUser) GetEmail() string              { return "test@example.com" }
func (u *fakeUser) GetRole() string               { return u.role }
func (u *fakeUser) GetPasswordHash() string       { return "" }
func (u *fakeUser) SetEmail(string)               {}
func (u *fakeUser) SetName(string)                {}
func (u *fakeUser) SetPasswordHash(string)        {}
func (u *fakeUser) SetRole(string)                {}
func (u *fakeUser) SetOrganizationID(uuid.UUID)   {}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite does not support gen_random_uuid(), so we create the table manually
	// and rely on the BeforeCreate hook to generate UUIDs.
	db.Exec(`CREATE TABLE IF NOT EXISTS test_products (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		name TEXT,
		price REAL
	)`)
	return db
}

func setupService(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	return New(Config{DB: db, Metadata: meta})
}

func newUser(orgID uuid.UUID) *fakeUser {
	return &fakeUser{id: uuid.New(), orgID: orgID}
}

func createProduct(t *testing.T, svc *Service, user *fakeUser, name string, price float64) map[string]any {
	t.Helper()
	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name":  name,
		"price": price,
	})
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCreate(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	out := createProduct(t, svc, user, "Widget", 9.99)
	if out["id"] == nil || out["id"] == "" {
		t.Fatal("expected returned data to contain an ID")
	}
	if out["name"] != "Widget" {
		t.Fatalf("name = %v, want Widget", out["name"])
	}
}

func TestGet(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	created := createProduct(t, svc, user, "Gadget", 19.99)
	id, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	got, err := svc.Get(context.Background(), "test_products", user, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["name"] != "Gadget" {
		t.Fatalf("name = %v, want Gadget", got["name"])
	}
}

func TestList(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	createProduct(t, svc, user, "A", 1)
	createProduct(t, svc, user, "B", 2)
	createProduct(t, svc, user, "C", 3)

	items, meta, err := svc.List(context.Background(), "test_products", user, query.Params{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3", len(items))
	}
	if meta.Total != 3 {
		t.Fatalf("total = %d, want 3", meta.Total)
	}
}

func TestUpdate(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	created := createProduct(t, svc, user, "Old", 5)
	id, _ := uuid.Parse(created["id"].(string))

	updated, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{"name": "New"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated["name"] != "New" {
		t.Fatalf("name = %v, want New", updated["name"])
	}
}

func TestDelete(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	created := createProduct(t, svc, user, "Doomed", 0)
	id, _ := uuid.Parse(created["id"].(string))

	if err := svc.Delete(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := svc.Get(context.Background(), "test_products", user, id)
	if err != ErrRecordNotFound {
		t.Fatalf("expected ErrRecordNotFound after delete, got %v", err)
	}
}

func TestTenantScoping(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)

	orgA := uuid.New()
	orgB := uuid.New()
	userA := newUser(orgA)
	userB := newUser(orgB)

	created := createProduct(t, svc, userA, "Secret", 42)
	id, _ := uuid.Parse(created["id"].(string))

	// userB should NOT see userA's record.
	_, err := svc.Get(context.Background(), "test_products", userB, id)
	if err != ErrRecordNotFound {
		t.Fatalf("expected ErrRecordNotFound for wrong org, got %v", err)
	}

	// List for userB should be empty.
	items, _, err := svc.List(context.Background(), "test_products", userB, query.Params{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items for org B, got %d", len(items))
	}
}

func TestModelNotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	_, _, err := svc.List(context.Background(), "nonexistent", user, query.Params{})
	if err == nil {
		t.Fatal("expected error for unregistered model")
	}
}

// ---------------------------------------------------------------------------
// Canonical event fan-out
// ---------------------------------------------------------------------------

// fanOutBus is a minimal in-test Publisher that records every Publish and
// forwards to handlers subscribed by exact event name. It mimics the
// fan-out semantics of *events.Bus without importing the events package
// (events → security → bundle → dynamic would close an import cycle).
type fanOutBus struct {
	mu        sync.Mutex
	published []publishedEvent
	subs      map[string][]func(context.Context, uuid.UUID, any) error
}

type publishedEvent struct {
	addonKey string
	event    string
	orgID    uuid.UUID
	payload  any
}

func newFanOutBus() *fanOutBus {
	return &fanOutBus{subs: make(map[string][]func(context.Context, uuid.UUID, any) error)}
}

// Subscribe binds a handler to an exact event name.
func (b *fanOutBus) Subscribe(event string, h func(context.Context, uuid.UUID, any) error) {
	b.mu.Lock()
	b.subs[event] = append(b.subs[event], h)
	b.mu.Unlock()
}

// Publish satisfies dynamic.Publisher.
func (b *fanOutBus) Publish(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) error {
	b.mu.Lock()
	b.published = append(b.published, publishedEvent{addonKey: addonKey, event: event, orgID: orgID, payload: payload})
	handlers := append([]func(context.Context, uuid.UUID, any) error(nil), b.subs[event]...)
	b.mu.Unlock()
	for _, h := range handlers {
		if err := h(ctx, orgID, payload); err != nil {
			return err
		}
	}
	return nil
}

// TestEvents_FanOut wires a Bus (Publisher) into the service and asserts the
// canonical "<addonKey>.<model>.<created|updated|deleted>" stream is fanned
// out to a subscriber with the {id, before?, after?} contract.
func TestEvents_FanOut(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})

	bus := newFanOutBus()
	const addonKey = "shop"

	type captured struct {
		action  string
		orgID   uuid.UUID
		payload *CanonicalEvent
	}
	var (
		mu       sync.Mutex
		received []captured
	)

	subscribe := func(action string) {
		bus.Subscribe(addonKey+".test_products."+action, func(_ context.Context, orgID uuid.UUID, payload any) error {
			ev, ok := payload.(*CanonicalEvent)
			if !ok {
				t.Errorf("payload type = %T, want *CanonicalEvent", payload)
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			received = append(received, captured{action: action, orgID: orgID, payload: ev})
			return nil
		})
	}
	subscribe("created")
	subscribe("updated")
	subscribe("deleted")

	svc := New(Config{
		DB:               db,
		Metadata:         meta,
		Bus:              bus,
		AddonKeyForModel: func(_ context.Context, _ string) string { return addonKey },
	})
	user := newUser(uuid.New())

	createdMap := createProduct(t, svc, user, "Widget", 9.99)
	id, err := uuid.Parse(createdMap["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	if _, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{"name": "Renamed"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if err := svc.Delete(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 3 {
		t.Fatalf("received %d events, want 3", len(received))
	}

	// 0: created — id + after, no before
	c := received[0]
	if c.action != "created" {
		t.Fatalf("event[0].action = %s, want created", c.action)
	}
	if c.orgID != user.GetOrganizationID() {
		t.Errorf("event[0].orgID = %s, want %s", c.orgID, user.GetOrganizationID())
	}
	if c.payload.ID != id.String() {
		t.Errorf("event[0].id = %s, want %s", c.payload.ID, id.String())
	}
	if c.payload.Before != nil {
		t.Errorf("event[0].before = %v, want nil", c.payload.Before)
	}
	if c.payload.After == nil || c.payload.After["name"] != "Widget" {
		t.Errorf("event[0].after.name = %v, want Widget", c.payload.After)
	}

	// 1: updated — id + before + after
	u := received[1]
	if u.action != "updated" {
		t.Fatalf("event[1].action = %s, want updated", u.action)
	}
	if u.payload.ID != id.String() {
		t.Errorf("event[1].id = %s, want %s", u.payload.ID, id.String())
	}
	if u.payload.Before == nil || u.payload.Before["name"] != "Widget" {
		t.Errorf("event[1].before.name = %v, want Widget", u.payload.Before)
	}
	if u.payload.After == nil || u.payload.After["name"] != "Renamed" {
		t.Errorf("event[1].after.name = %v, want Renamed", u.payload.After)
	}

	// 2: deleted — id + before, no after
	d := received[2]
	if d.action != "deleted" {
		t.Fatalf("event[2].action = %s, want deleted", d.action)
	}
	if d.payload.ID != id.String() {
		t.Errorf("event[2].id = %s, want %s", d.payload.ID, id.String())
	}
	if d.payload.Before == nil || d.payload.Before["name"] != "Renamed" {
		t.Errorf("event[2].before.name = %v, want Renamed", d.payload.Before)
	}
	if d.payload.After != nil {
		t.Errorf("event[2].after = %v, want nil", d.payload.After)
	}

	// Verify Model/Action/AddonKey/ActorID envelope fields on each event.
	wantActions := []string{"created", "updated", "deleted"}
	for i, r := range received {
		ev := r.payload
		if ev.Model != "test_products" {
			t.Errorf("event[%d].Model = %q, want test_products", i, ev.Model)
		}
		if ev.Action != wantActions[i] {
			t.Errorf("event[%d].Action = %q, want %q", i, ev.Action, wantActions[i])
		}
		if ev.AddonKey != addonKey {
			t.Errorf("event[%d].AddonKey = %q, want %q", i, ev.AddonKey, addonKey)
		}
		if ev.ActorID != user.GetID().String() {
			t.Errorf("event[%d].ActorID = %q, want %q", i, ev.ActorID, user.GetID().String())
		}
	}

	// Verify the producer addonKey + event names also reached Publish — proves
	// the wiring uses the resolver, not just hardcoded "kernel".
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if len(bus.published) != 3 {
		t.Fatalf("bus.published = %d, want 3", len(bus.published))
	}
	wantNames := []string{"shop.test_products.created", "shop.test_products.updated", "shop.test_products.deleted"}
	for i, want := range wantNames {
		if bus.published[i].event != want {
			t.Errorf("published[%d].event = %s, want %s", i, bus.published[i].event, want)
		}
		if bus.published[i].addonKey != "shop" {
			t.Errorf("published[%d].addonKey = %s, want shop", i, bus.published[i].addonKey)
		}
	}
}

// TestEvents_NoBusIsNoop confirms a Service without a wired Bus keeps the
// pre-event behaviour: CRUD calls succeed and nothing panics.
func TestEvents_NoBusIsNoop(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db) // no Bus
	user := newUser(uuid.New())

	created := createProduct(t, svc, user, "Quiet", 1.0)
	id, _ := uuid.Parse(created["id"].(string))
	if _, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{"name": "Still quiet"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := svc.Delete(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// TestEvents_CorrelationIDPropagation verifies that a correlation ID injected
// into the context via WithCorrelationID is surfaced on the published
// CanonicalEvent and that CorrelationIDFromContext returns "" on a plain ctx.
func TestEvents_CorrelationIDPropagation(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	bus := newFanOutBus()

	var captured *CanonicalEvent
	bus.Subscribe("kernel.test_products.created", func(_ context.Context, _ uuid.UUID, payload any) error {
		captured, _ = payload.(*CanonicalEvent)
		return nil
	})

	svc := New(Config{DB: db, Metadata: meta, Bus: bus})
	user := newUser(uuid.New())

	const wantCorrID = "req-abc-123"
	ctx := WithCorrelationID(context.Background(), wantCorrID)

	if _, err := svc.Create(ctx, "test_products", user, map[string]any{"name": "Correlated", "price": 1.0}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if captured == nil {
		t.Fatal("no event received")
	}
	if captured.CorrelationID != wantCorrID {
		t.Errorf("CorrelationID = %q, want %q", captured.CorrelationID, wantCorrID)
	}

	// A plain context should produce an empty CorrelationID.
	if got := CorrelationIDFromContext(context.Background()); got != "" {
		t.Errorf("CorrelationIDFromContext(plain) = %q, want empty", got)
	}
}

// TestEvents_NoCorrelationID verifies that events published without an
// enriched context leave CorrelationID empty (not a panic, not a placeholder).
func TestEvents_NoCorrelationID(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	bus := newFanOutBus()

	var captured *CanonicalEvent
	bus.Subscribe("kernel.test_products.created", func(_ context.Context, _ uuid.UUID, payload any) error {
		captured, _ = payload.(*CanonicalEvent)
		return nil
	})

	svc := New(Config{DB: db, Metadata: meta, Bus: bus})
	user := newUser(uuid.New())

	if _, err := svc.Create(context.Background(), "test_products", user, map[string]any{"name": "No corr", "price": 2.0}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if captured == nil {
		t.Fatal("no event received")
	}
	if captured.CorrelationID != "" {
		t.Errorf("CorrelationID = %q, want empty string", captured.CorrelationID)
	}
}

// TestEvents_BackwardCompatFields guards the ID/Before/After contract relied on
// by existing consumers (e.g. ops services/sales_stock_effects.go). The new
// envelope fields must not displace or rename these.
func TestEvents_BackwardCompatFields(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	bus := newFanOutBus()

	var createdEv, updatedEv, deletedEv *CanonicalEvent
	bus.Subscribe("kernel.test_products.created", func(_ context.Context, _ uuid.UUID, p any) error {
		createdEv, _ = p.(*CanonicalEvent)
		return nil
	})
	bus.Subscribe("kernel.test_products.updated", func(_ context.Context, _ uuid.UUID, p any) error {
		updatedEv, _ = p.(*CanonicalEvent)
		return nil
	})
	bus.Subscribe("kernel.test_products.deleted", func(_ context.Context, _ uuid.UUID, p any) error {
		deletedEv, _ = p.(*CanonicalEvent)
		return nil
	})

	svc := New(Config{DB: db, Metadata: meta, Bus: bus})
	user := newUser(uuid.New())
	ctx := context.Background()

	createdMap := createProduct(t, svc, user, "Compat", 5.0)
	id, _ := uuid.Parse(createdMap["id"].(string))

	if _, err := svc.Update(ctx, "test_products", user, id, map[string]any{"name": "CompatRenamed"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := svc.Delete(ctx, "test_products", user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// created: ID set, Before nil, After contains the row.
	if createdEv == nil {
		t.Fatal("createdEv is nil")
	}
	if createdEv.ID != id.String() {
		t.Errorf("created.ID = %q, want %q", createdEv.ID, id.String())
	}
	if createdEv.Before != nil {
		t.Errorf("created.Before = %v, want nil", createdEv.Before)
	}
	if createdEv.After == nil {
		t.Error("created.After is nil, want map with row data")
	}

	// updated: ID set, Before contains old name, After contains new name.
	if updatedEv == nil {
		t.Fatal("updatedEv is nil")
	}
	if updatedEv.ID != id.String() {
		t.Errorf("updated.ID = %q, want %q", updatedEv.ID, id.String())
	}
	if updatedEv.Before == nil || updatedEv.Before["name"] != "Compat" {
		t.Errorf("updated.Before[name] = %v, want Compat", updatedEv.Before["name"])
	}
	if updatedEv.After == nil || updatedEv.After["name"] != "CompatRenamed" {
		t.Errorf("updated.After[name] = %v, want CompatRenamed", updatedEv.After["name"])
	}

	// deleted: ID set, Before contains last-known row, After nil.
	if deletedEv == nil {
		t.Fatal("deletedEv is nil")
	}
	if deletedEv.ID != id.String() {
		t.Errorf("deleted.ID = %q, want %q", deletedEv.ID, id.String())
	}
	if deletedEv.Before == nil {
		t.Error("deleted.Before is nil, want pre-delete snapshot")
	}
	if deletedEv.After != nil {
		t.Errorf("deleted.After = %v, want nil", deletedEv.After)
	}
}

// ---------------------------------------------------------------------------
// ModelResolver wiring — guards against regressions where resolveModel
// bypasses Config.ModelResolver and reads modelbase.Get directly. Hosts that
// keep their own model registry (e.g. meta-core/models) depend on this.
// ---------------------------------------------------------------------------

// TestModelResolver_CustomResolverInvoked wires a Config.ModelResolver and
// confirms every CRUD path drives lookups through it. Pre-fix, resolveModel
// called modelbase.Get directly — the resolver would never fire and `hits`
// would be 0.
//
// We still register the model in modelbase so metadata.Service.GetTable (a
// separate component the test does not exercise the fix on) can resolve the
// schema. The assertion that proves the fix is `hits > 0`, not the absence
// of a modelbase entry.
func TestModelResolver_CustomResolverInvoked(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})

	var hits int
	resolver := func(_ context.Context, name string) (any, bool) {
		hits++
		// Delegate to modelbase for the underlying instance so storage works,
		// but the call itself proves the dynamic service is honouring the
		// configured resolver instead of bypassing it.
		inst, ok := modelbase.Get(name)
		if !ok {
			return nil, false
		}
		return inst, true
	}

	svc := New(Config{DB: db, Metadata: meta, ModelResolver: resolver})
	user := newUser(uuid.New())

	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name":  "FromResolver",
		"price": 1.23,
	})
	if err != nil {
		t.Fatalf("create via resolver: %v", err)
	}
	if hits == 0 {
		t.Fatal("expected ModelResolver to be invoked on Create, got 0 hits — resolveModel is bypassing Config.ModelResolver")
	}
	id, err := uuid.Parse(out["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	// Reset and walk every CRUD path so each resolveModel call is observed.
	hits = 0
	if _, err := svc.Get(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("get via resolver: %v", err)
	}
	if _, _, err := svc.List(context.Background(), "test_products", user, query.Params{Page: 1, PerPage: 10}); err != nil {
		t.Fatalf("list via resolver: %v", err)
	}
	if _, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{"name": "Renamed"}); err != nil {
		t.Fatalf("update via resolver: %v", err)
	}
	if err := svc.Delete(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("delete via resolver: %v", err)
	}
	if hits < 4 {
		t.Fatalf("expected resolver to be called once per Get/List/Update/Delete (>=4), got %d", hits)
	}
}

// TestModelResolver_BackwardCompatModelbase confirms that when no
// Config.ModelResolver is wired, resolveModel still falls back to
// modelbase.Get — a host that has not opted in to the resolver keeps
// working exactly as before.
func TestModelResolver_BackwardCompatModelbase(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db) // no ModelResolver in Config
	user := newUser(uuid.New())

	// test_products is registered in modelbase by setupService; this would
	// fail with ErrModelNotFound if the fallback to modelbase.Get were
	// dropped.
	created := createProduct(t, svc, user, "FromModelbase", 4.2)
	if created["name"] != "FromModelbase" {
		t.Fatalf("name = %v, want FromModelbase", created["name"])
	}
}

// TestModelResolver_ReturnsFalseYieldsNotFound asserts that when the wired
// resolver signals "unknown model" (ok == false), the service surfaces
// ErrModelNotFound and does NOT silently fall back to modelbase.Get.
func TestModelResolver_ReturnsFalseYieldsNotFound(t *testing.T) {
	db := setupTestDB(t)
	// Register test_products in modelbase to prove the resolver overrides it:
	// if the service ever fell through to modelbase.Get the call would
	// succeed instead of returning ErrModelNotFound.
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})

	resolver := func(_ context.Context, _ string) (any, bool) { return nil, false }
	svc := New(Config{DB: db, Metadata: meta, ModelResolver: resolver})
	user := newUser(uuid.New())

	_, _, err := svc.List(context.Background(), "test_products", user, query.Params{Page: 1, PerPage: 10})
	if err != ErrModelNotFound {
		t.Fatalf("expected ErrModelNotFound when resolver returns false, got %v", err)
	}
}

// TestEvents_DefaultAddonKeyKernel verifies that when no AddonKeyForModel
// resolver is wired, events are namespaced under "kernel.<model>.<action>".
func TestEvents_DefaultAddonKeyKernel(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})

	bus := newFanOutBus()
	var hits int
	bus.Subscribe("kernel.test_products.created", func(_ context.Context, _ uuid.UUID, _ any) error {
		hits++
		return nil
	})

	svc := New(Config{DB: db, Metadata: meta, Bus: bus}) // no AddonKeyForModel
	user := newUser(uuid.New())

	createProduct(t, svc, user, "X", 1)
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}

// dynItemsMeta is a compiled twin used ONLY so metadata.GetTable can resolve
// the "dyn_items" model. The instance the CRUD paths actually operate on is a
// reflect-built struct with no Go methods (see TestTableNameResolver_*), which
// is exactly the case real addon models hit.
type dynItemsMeta struct {
	modelbase.BaseUUIDModel
	Label string `json:"label"`
}

func (dynItemsMeta) TableName() string { return "dyn_items" }
func (dynItemsMeta) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title:   "Dyn Items",
		Columns: []modelbase.ColumnDef{{Key: "label", Label: "Label"}},
	}
}
func (dynItemsMeta) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Dyn Item"}
}

// reflectDynType builds a runtime struct mirroring an addon model: it has the
// right json/gorm tags but — like every reflect.StructOf type — carries NO Go
// methods, so it cannot implement modelbase.ModelDefiner. Its table can only be
// recovered through Config.TableNameResolver.
func reflectDynType() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "ID", Type: reflect.TypeOf(""), Tag: `json:"id" gorm:"column:id;primaryKey"`},
		{Name: "OrganizationID", Type: reflect.TypeOf(""), Tag: `json:"organization_id" gorm:"column:organization_id"`},
		{Name: "CreatedByID", Type: reflect.TypeOf(""), Tag: `json:"created_by_id" gorm:"column:created_by_id"`},
		{Name: "Label", Type: reflect.TypeOf(""), Tag: `json:"label" gorm:"column:label"`},
	})
}

// TestTableNameResolver_MethodlessModel is the core of the kernel→host
// migration: an addon model whose instance has no TableName() method must still
// route every CRUD path to the right table via Config.TableNameResolver. Before
// this, the service asserted instance.(modelbase.ModelDefiner) and panicked.
func TestTableNameResolver_MethodlessModel(t *testing.T) {
	db := setupTestDB(t)
	db.Exec(`CREATE TABLE IF NOT EXISTS dyn_items (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		label TEXT
	)`)
	// Compiled twin for metadata resolution only.
	modelbase.Register("dyn_items", func() modelbase.ModelDefiner { return &dynItemsMeta{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})

	dynType := reflectDynType()
	resolver := func(_ context.Context, name string) (any, bool) {
		if name != "dyn_items" {
			return nil, false
		}
		// Fresh method-less instance per call — proves the assertion is gone.
		return reflect.New(dynType).Interface(), true
	}
	var tableHits int
	tableResolver := func(_ context.Context, name string) (string, bool) {
		if name == "dyn_items" {
			tableHits++
			return "dyn_items", true
		}
		return "", false
	}

	svc := New(Config{
		DB:                db,
		Metadata:          meta,
		ModelResolver:     resolver,
		TableNameResolver: tableResolver,
	})
	user := newUser(uuid.New())
	ctx := context.Background()

	// Create — reflect structs get no BeforeCreate hook, so supply the id.
	id := uuid.New()
	created, err := svc.Create(ctx, "dyn_items", user, map[string]any{
		"id":    id.String(),
		"label": "alpha",
	})
	if err != nil {
		t.Fatalf("create method-less model: %v", err)
	}
	if created["label"] != "alpha" {
		t.Fatalf("label = %v, want alpha", created["label"])
	}

	// Get
	got, err := svc.Get(ctx, "dyn_items", user, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["label"] != "alpha" {
		t.Fatalf("get label = %v, want alpha", got["label"])
	}

	// List — this is the exact path that 500'd in the host ("model type not
	// found in query"): it must return the row, not blow up.
	items, pm, err := svc.List(ctx, "dyn_items", user, query.Params{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || pm.Total != 1 {
		t.Fatalf("list len/total = %d/%d, want 1/1", len(items), pm.Total)
	}

	// Update
	if _, err := svc.Update(ctx, "dyn_items", user, id, map[string]any{"label": "beta"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	reGot, _ := svc.Get(ctx, "dyn_items", user, id)
	if reGot["label"] != "beta" {
		t.Fatalf("after update label = %v, want beta", reGot["label"])
	}

	// Delete
	if err := svc.Delete(ctx, "dyn_items", user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(ctx, "dyn_items", user, id); err != ErrRecordNotFound {
		t.Fatalf("after delete, get = %v, want ErrRecordNotFound", err)
	}

	if tableHits == 0 {
		t.Fatal("TableNameResolver was never consulted — the CRUD paths are not using it")
	}
}

// TestTableNameResolver_FallsBackToModelDefiner confirms the new resolver is
// strictly additive: when no TableNameResolver is wired, a model that DOES
// implement ModelDefiner still resolves its table the old way.
func TestTableNameResolver_FallsBackToModelDefiner(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db) // no TableNameResolver
	user := newUser(uuid.New())

	created := createProduct(t, svc, user, "Compiled", 7)
	if created["name"] != "Compiled" {
		t.Fatalf("name = %v, want Compiled", created["name"])
	}
}
