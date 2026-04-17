package dynamic

import (
	"context"
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
