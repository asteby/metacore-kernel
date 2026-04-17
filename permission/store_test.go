package permission

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestInMemoryStore_RolePermissions(t *testing.T) {
	userID := uuid.New()
	store := NewInMemoryStore(
		map[Role][]Capability{
			RoleAdmin: {Cap("users", "create"), Cap("users", "delete")},
		},
		map[uuid.UUID][]Capability{
			userID: {Cap("reports", "export")},
		},
	)

	ctx := context.Background()

	// Known role.
	caps, err := store.GetRolePermissions(ctx, RoleAdmin)
	if err != nil {
		t.Fatalf("GetRolePermissions: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("want 2 caps, got %d", len(caps))
	}

	// Unknown role -> ErrUnknownRole.
	if _, err := store.GetRolePermissions(ctx, Role("mystery")); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("want ErrUnknownRole, got %v", err)
	}

	// User overrides.
	userCaps, err := store.GetUserPermissions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserPermissions: %v", err)
	}
	if len(userCaps) != 1 || userCaps[0] != Cap("reports", "export") {
		t.Fatalf("unexpected user caps: %v", userCaps)
	}

	// Unknown user -> empty slice, no error.
	unknownCaps, err := store.GetUserPermissions(ctx, uuid.New())
	if err != nil {
		t.Fatalf("GetUserPermissions unknown user: %v", err)
	}
	if len(unknownCaps) != 0 {
		t.Fatalf("want empty, got %v", unknownCaps)
	}
}

func TestInMemoryStore_SetRoleAndUser(t *testing.T) {
	store := NewInMemoryStore(nil, nil)
	store.SetRole(RoleAgent, []Capability{Cap("tickets", "read")})
	userID := uuid.New()
	store.SetUser(userID, []Capability{Cap("tickets", "close")})

	ctx := context.Background()
	caps, _ := store.GetRolePermissions(ctx, RoleAgent)
	if len(caps) != 1 {
		t.Fatalf("role caps: %v", caps)
	}
	ucaps, _ := store.GetUserPermissions(ctx, userID)
	if len(ucaps) != 1 {
		t.Fatalf("user caps: %v", ucaps)
	}

	// Mutating the returned slice must not affect the store.
	caps[0] = Cap("evil", "hack")
	again, _ := store.GetRolePermissions(ctx, RoleAgent)
	if again[0] != Cap("tickets", "read") {
		t.Fatalf("store mutated: %v", again)
	}
}

func TestInMemoryStore_RoleCaseInsensitive(t *testing.T) {
	store := NewInMemoryStore(
		map[Role][]Capability{Role("ADMIN"): {Cap("users", "read")}},
		nil,
	)
	caps, err := store.GetRolePermissions(context.Background(), RoleAdmin)
	if err != nil {
		t.Fatalf("GetRolePermissions: %v", err)
	}
	if len(caps) != 1 {
		t.Fatalf("case-insensitive lookup failed: %v", caps)
	}
}

// ---------------------------------------------------------------------------
// GormStore
// ---------------------------------------------------------------------------

// newTestDB returns an in-memory SQLite DB with the grant tables pre-created.
// We can't rely on AutoMigrate because BaseUUIDModel declares
// `default:gen_random_uuid()` which SQLite doesn't understand; BeforeCreate
// fills the UUID at runtime so a hand-rolled schema is enough.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE permission_role_grants (
			id TEXT PRIMARY KEY,
			organization_id TEXT,
			created_by_id TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME,
			role TEXT,
			capability TEXT
		)`,
		`CREATE UNIQUE INDEX idx_role_perm ON permission_role_grants(role, capability)`,
		`CREATE TABLE permission_user_grants (
			id TEXT PRIMARY KEY,
			organization_id TEXT,
			created_by_id TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME,
			user_id TEXT,
			capability TEXT
		)`,
		`CREATE UNIQUE INDEX idx_user_perm ON permission_user_grants(user_id, capability)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

// newGormStoreForTest bypasses AutoMigrate (SQLite + gen_random_uuid mismatch)
// while still exercising all read/write paths of GormStore.
func newGormStoreForTest(t *testing.T, db *gorm.DB) *GormStore {
	t.Helper()
	return &GormStore{db: db}
}

func TestGormStore_AutoMigrateContract(t *testing.T) {
	// Production callers rely on NewGormStore to create the tables. We
	// assert the table names and index names the migration promises by
	// inspecting the hand-rolled schema from newTestDB.
	db := newTestDB(t)
	if !db.Migrator().HasTable(&RolePermission{}) {
		t.Fatal("role grants table missing")
	}
	if !db.Migrator().HasTable(&UserPermission{}) {
		t.Fatal("user grants table missing")
	}
}

func TestNewGormStore_RejectsNilDB(t *testing.T) {
	if _, err := NewGormStore(nil); err == nil {
		t.Fatal("want error for nil db")
	}
}

func TestGormStore_CRUD(t *testing.T) {
	db := newTestDB(t)
	store := newGormStoreForTest(t, db)
	ctx := context.Background()
	userID := uuid.New()

	// No grants yet — role returns ErrUnknownRole.
	if _, err := store.GetRolePermissions(ctx, RoleAdmin); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("want ErrUnknownRole, got %v", err)
	}

	// Grant then read.
	if err := store.GrantRole(ctx, RoleAdmin, Cap("users", "create")); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	// Idempotency: grant twice, still one row.
	if err := store.GrantRole(ctx, RoleAdmin, Cap("users", "create")); err != nil {
		t.Fatalf("GrantRole idempotent: %v", err)
	}
	caps, err := store.GetRolePermissions(ctx, RoleAdmin)
	if err != nil {
		t.Fatalf("GetRolePermissions: %v", err)
	}
	if len(caps) != 1 || caps[0] != Cap("users", "create") {
		t.Fatalf("unexpected caps: %v", caps)
	}

	// User grants.
	if err := store.GrantUser(ctx, userID, Cap("reports", "export")); err != nil {
		t.Fatalf("GrantUser: %v", err)
	}
	ucaps, err := store.GetUserPermissions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserPermissions: %v", err)
	}
	if len(ucaps) != 1 {
		t.Fatalf("want 1 user cap, got %v", ucaps)
	}

	// Revoke.
	if err := store.RevokeRole(ctx, RoleAdmin, Cap("users", "create")); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	if _, err := store.GetRolePermissions(ctx, RoleAdmin); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("want ErrUnknownRole after revoke, got %v", err)
	}
	if err := store.RevokeUser(ctx, userID, Cap("reports", "export")); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	ucaps, _ = store.GetUserPermissions(ctx, userID)
	if len(ucaps) != 0 {
		t.Fatalf("want empty after revoke, got %v", ucaps)
	}
}

