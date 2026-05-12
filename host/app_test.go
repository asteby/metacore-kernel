package host

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/modelbase"
)

// hostTestProduct is a minimal model registered for the duration of a single
// test so we can drive a Create through app.Dynamic and observe the canonical
// event fan-out on app.Bus.
type hostTestProduct struct {
	modelbase.BaseUUIDModel
	Name string `json:"name" gorm:"size:255"`
}

func (hostTestProduct) TableName() string { return "host_test_products" }
func (hostTestProduct) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Host Test Products",
		Columns: []modelbase.ColumnDef{
			{Key: "name", Label: "Name"},
		},
	}
}
func (hostTestProduct) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Host Test Product"}
}

type hostFakeUser struct {
	id    uuid.UUID
	orgID uuid.UUID
}

func (u *hostFakeUser) GetID() uuid.UUID            { return u.id }
func (u *hostFakeUser) GetOrganizationID() uuid.UUID { return u.orgID }
func (u *hostFakeUser) GetEmail() string             { return "host-test@example.com" }
func (u *hostFakeUser) GetRole() string              { return "admin" }
func (u *hostFakeUser) GetPasswordHash() string      { return "" }
func (u *hostFakeUser) SetEmail(string)              {}
func (u *hostFakeUser) SetName(string)               {}
func (u *hostFakeUser) SetPasswordHash(string)       {}
func (u *hostFakeUser) SetRole(string)               {}
func (u *hostFakeUser) SetOrganizationID(uuid.UUID)  {}

func setupHostTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS host_test_products (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		name TEXT
	)`)
	return db
}

// TestApp_BusWiredIntoDynamic confirms NewApp constructs an events.Bus and
// passes it to dynamic.Service so a CRUD mutation routed through app.Dynamic
// fans out a canonical event to subscribers registered on app.Bus.
//
// Regression guard for the bug that the 2026-05-04 dynamic-events audit
// flagged: dynamic.New(...) used to be constructed without Bus, turning every
// publishCanonical call into a no-op.
func TestApp_BusWiredIntoDynamic(t *testing.T) {
	db := setupHostTestDB(t)
	modelbase.Register("host_test_products", func() modelbase.ModelDefiner { return &hostTestProduct{} })

	app := NewApp(AppConfig{
		DB:        db,
		JWTSecret: []byte("test-secret-32-bytes-long-xxxxxx"),
	})

	if app.Bus == nil {
		t.Fatal("app.Bus is nil — NewApp must construct an events.Bus")
	}

	var (
		mu       sync.Mutex
		hits     int
		gotEvent string
	)
	if err := app.Bus.Subscribe("kernel", "kernel.host_test_products.created", func(_ context.Context, _ uuid.UUID, _ any) error {
		mu.Lock()
		defer mu.Unlock()
		hits++
		gotEvent = "kernel.host_test_products.created"
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	user := &hostFakeUser{id: uuid.New(), orgID: uuid.New()}
	if _, err := app.Dynamic.Create(context.Background(), "host_test_products", user, map[string]any{
		"name": "Widget",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("subscriber hits = %d, want 1 — app.Dynamic did not publish to app.Bus", hits)
	}
	if gotEvent != "kernel.host_test_products.created" {
		t.Fatalf("event name = %q, want kernel.host_test_products.created", gotEvent)
	}
}

// TestApp_AddonKeyForModelResolverFlowsThrough confirms an AddonKeyForModel
// passed via AppConfig reaches the dynamic.Service event publisher, so addons
// can namespace their canonical events with the addon owner of the model.
func TestApp_AddonKeyForModelResolverFlowsThrough(t *testing.T) {
	db := setupHostTestDB(t)
	modelbase.Register("host_test_products", func() modelbase.ModelDefiner { return &hostTestProduct{} })

	app := NewApp(AppConfig{
		DB:        db,
		JWTSecret: []byte("test-secret-32-bytes-long-xxxxxx"),
		AddonKeyForModel: func(_ context.Context, model string) string {
			if model == "host_test_products" {
				return "shop"
			}
			return ""
		},
	})

	var hits int
	if err := app.Bus.Subscribe("kernel", "shop.host_test_products.created", func(_ context.Context, _ uuid.UUID, _ any) error {
		hits++
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	user := &hostFakeUser{id: uuid.New(), orgID: uuid.New()}
	if _, err := app.Dynamic.Create(context.Background(), "host_test_products", user, map[string]any{
		"name": "Widget",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if hits != 1 {
		t.Fatalf("subscriber hits = %d, want 1 — AddonKeyForModel did not flow into dynamic.Service", hits)
	}
}
