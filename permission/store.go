package permission

import (
	"context"
	"errors"
	"sync"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// PermissionStore is the stable contract for reading capability grants. Apps
// with exotic requirements (branch-scoped permissions, Redis cache, addon
// policy engines) implement this interface; the kernel's Service is
// oblivious to the storage choice.
//
// Implementations MUST be safe for concurrent use. They SHOULD return
// ErrUnknownRole (not a generic error) when asked about a role they do not
// recognise so the service can treat it as "no grants" rather than a 5xx.
type PermissionStore interface {
	// GetRolePermissions returns the capabilities attached to role. If the
	// role is unknown, an implementation may either return an empty slice or
	// ErrUnknownRole; the service treats both the same.
	GetRolePermissions(ctx context.Context, role Role) ([]Capability, error)

	// GetUserPermissions returns the capabilities attached directly to
	// userID (i.e. per-user overrides in addition to their role). Returns an
	// empty slice when the user has no overrides.
	GetUserPermissions(ctx context.Context, userID uuid.UUID) ([]Capability, error)
}

// ---------------------------------------------------------------------------
// InMemoryStore
// ---------------------------------------------------------------------------

// InMemoryStore is the default PermissionStore for tests and apps that can
// declare their policy at boot. It holds two maps guarded by a RWMutex.
// Zero-value maps are valid — missing keys simply return empty slices.
type InMemoryStore struct {
	mu       sync.RWMutex
	roleCaps map[Role][]Capability
	userCaps map[uuid.UUID][]Capability
}

// NewInMemoryStore returns a store seeded with the supplied maps. Passing nil
// for either argument is equivalent to an empty map; callers then populate
// via SetRole / SetUser as they register models at boot.
func NewInMemoryStore(roleCaps map[Role][]Capability, userCaps map[uuid.UUID][]Capability) *InMemoryStore {
	s := &InMemoryStore{
		roleCaps: make(map[Role][]Capability),
		userCaps: make(map[uuid.UUID][]Capability),
	}
	for r, caps := range roleCaps {
		s.roleCaps[NormalizeRole(r.String())] = append([]Capability(nil), caps...)
	}
	for u, caps := range userCaps {
		s.userCaps[u] = append([]Capability(nil), caps...)
	}
	return s
}

// GetRolePermissions implements PermissionStore.
func (s *InMemoryStore) GetRolePermissions(_ context.Context, role Role) ([]Capability, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	caps, ok := s.roleCaps[NormalizeRole(role.String())]
	if !ok {
		// Return empty + ErrUnknownRole; the service treats it as "no grants"
		// and apps that care (e.g. an admin UI) can distinguish.
		return nil, ErrUnknownRole
	}
	out := make([]Capability, len(caps))
	copy(out, caps)
	return out, nil
}

// GetUserPermissions implements PermissionStore.
func (s *InMemoryStore) GetUserPermissions(_ context.Context, userID uuid.UUID) ([]Capability, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	caps := s.userCaps[userID]
	out := make([]Capability, len(caps))
	copy(out, caps)
	return out, nil
}

// SetRole replaces the capability list for role. Used by tests and apps that
// let admins edit role grants in memory.
func (s *InMemoryStore) SetRole(role Role, caps []Capability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roleCaps[NormalizeRole(role.String())] = append([]Capability(nil), caps...)
}

// SetUser replaces the per-user capability overrides for userID.
func (s *InMemoryStore) SetUser(userID uuid.UUID, caps []Capability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userCaps[userID] = append([]Capability(nil), caps...)
}

// ---------------------------------------------------------------------------
// GormStore
// ---------------------------------------------------------------------------

// RolePermission is the persisted row shape for role -> capability grants. It
// embeds BaseUUIDModel so each grant is tenant-scoped (OrganizationID) and
// carries audit metadata; the kernel itself ignores OrganizationID here
// because the Service signature is scoped by user, and apps that need
// cross-org isolation enforce it at the store boundary via their own
// implementation.
type RolePermission struct {
	modelbase.BaseUUIDModel
	Role       Role       `json:"role" gorm:"size:50;index:idx_role_perm,unique"`
	Capability Capability `json:"capability" gorm:"size:255;index:idx_role_perm,unique"`
}

// TableName pins the table name so apps do not end up with plural
// "role_permissions" vs "role_permission" drift between services.
func (RolePermission) TableName() string { return "permission_role_grants" }

// UserPermission stores a per-user capability override (e.g. an extra grant
// that supersedes the user's role grants).
type UserPermission struct {
	modelbase.BaseUUIDModel
	UserID     uuid.UUID  `json:"user_id" gorm:"type:uuid;index:idx_user_perm,unique"`
	Capability Capability `json:"capability" gorm:"size:255;index:idx_user_perm,unique"`
}

// TableName pins the table name; see RolePermission for rationale.
func (UserPermission) TableName() string { return "permission_user_grants" }

// GormStore is a GORM-backed PermissionStore that AutoMigrates its two tables
// and implements the interface via straightforward queries. It is the
// recommended default for apps that already use GORM for auth; apps needing
// Redis caching or custom scoping are expected to wrap it or replace it.
type GormStore struct {
	db *gorm.DB
}

// NewGormStore connects store to db and runs AutoMigrate for both grant
// tables. Returns an error if migration fails so the caller can abort boot.
func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if db == nil {
		return nil, errors.New("permission: nil *gorm.DB")
	}
	if err := db.AutoMigrate(&RolePermission{}, &UserPermission{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

// GetRolePermissions implements PermissionStore. Returns ErrUnknownRole when
// zero rows match so callers can distinguish "role exists but has no grants"
// from "role never seen" — GORM has no native concept of that so we use row
// count as proxy.
func (s *GormStore) GetRolePermissions(ctx context.Context, role Role) ([]Capability, error) {
	var rows []RolePermission
	err := s.db.WithContext(ctx).
		Where("role = ?", NormalizeRole(role.String())).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrUnknownRole
	}
	out := make([]Capability, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Capability)
	}
	return out, nil
}

// GetUserPermissions implements PermissionStore.
func (s *GormStore) GetUserPermissions(ctx context.Context, userID uuid.UUID) ([]Capability, error) {
	var rows []UserPermission
	err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]Capability, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Capability)
	}
	return out, nil
}

// GrantRole inserts a (role, cap) row. Idempotent — if the unique index
// already has the pair, the call is treated as a no-op. Convenience for
// bootstrapping and tests; not part of PermissionStore.
func (s *GormStore) GrantRole(ctx context.Context, role Role, cap Capability) error {
	r := RolePermission{Role: NormalizeRole(role.String()), Capability: cap}
	err := s.db.WithContext(ctx).
		Where("role = ? AND capability = ?", r.Role, r.Capability).
		FirstOrCreate(&r).Error
	return err
}

// GrantUser inserts a (user, cap) row. See GrantRole.
func (s *GormStore) GrantUser(ctx context.Context, userID uuid.UUID, cap Capability) error {
	u := UserPermission{UserID: userID, Capability: cap}
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND capability = ?", userID, cap).
		FirstOrCreate(&u).Error
	return err
}

// RevokeRole deletes the (role, cap) row if present.
func (s *GormStore) RevokeRole(ctx context.Context, role Role, cap Capability) error {
	return s.db.WithContext(ctx).
		Where("role = ? AND capability = ?", NormalizeRole(role.String()), cap).
		Delete(&RolePermission{}).Error
}

// RevokeUser deletes the (user, cap) row if present.
func (s *GormStore) RevokeUser(ctx context.Context, userID uuid.UUID, cap Capability) error {
	return s.db.WithContext(ctx).
		Where("user_id = ? AND capability = ?", userID, cap).
		Delete(&UserPermission{}).Error
}

// Compile-time interface assertions.
var (
	_ PermissionStore = (*InMemoryStore)(nil)
	_ PermissionStore = (*GormStore)(nil)
)
