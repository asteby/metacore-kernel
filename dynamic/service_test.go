package dynamic

import (
	"context"
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
