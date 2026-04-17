package modelbase

import (
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Canonical role names. Apps MAY define additional roles, but owner/admin/agent
// are the baseline recognised by the kernel's default permission policies.
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
	RoleAgent = "agent"
)

// BaseUser is the canonical principal shape. Apps extend it by embedding:
//
//	type User struct {
//	    modelbase.BaseUser
//	    BranchID *uuid.UUID `json:"branch_id,omitempty" gorm:"type:uuid;index"`
//	}
//
// The GORM tags match the ops backend exactly so AutoMigrate remains
// interchangeable between kernel-consumers.
type BaseUser struct {
	BaseUUIDModel
	Name         string     `json:"name" gorm:"size:255"`
	Email        string     `json:"email" gorm:"uniqueIndex:idx_users_email;size:255"`
	PasswordHash string     `json:"-" gorm:"size:255"`
	Role         string     `json:"role" gorm:"default:'agent';index;size:50"` // owner|admin|agent
	Avatar       string     `json:"avatar,omitempty" gorm:"size:500"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// TableName is fixed because the principal table is canonical across apps; if
// an app needs a different table, it should define its own model rather than
// shadowing TableName.
func (u *BaseUser) TableName() string { return "users" }

// GetID satisfies AuthUser.
func (u *BaseUser) GetID() uuid.UUID { return u.ID }

// GetOrganizationID satisfies AuthUser.
func (u *BaseUser) GetOrganizationID() uuid.UUID { return u.OrganizationID }

// GetEmail satisfies AuthUser.
func (u *BaseUser) GetEmail() string { return u.Email }

// GetRole satisfies AuthUser.
func (u *BaseUser) GetRole() string { return u.Role }

// GetPasswordHash satisfies AuthUser.
func (u *BaseUser) GetPasswordHash() string { return u.PasswordHash }

// SetEmail satisfies AuthUser.
func (u *BaseUser) SetEmail(v string) { u.Email = v }

// SetName satisfies AuthUser.
func (u *BaseUser) SetName(v string) { u.Name = v }

// SetPasswordHash satisfies AuthUser. Callers MUST pass an already-hashed
// value (e.g. from auth.HashPassword); use SetPassword for the plaintext path.
func (u *BaseUser) SetPasswordHash(v string) { u.PasswordHash = v }

// SetRole satisfies AuthUser.
func (u *BaseUser) SetRole(v string) { u.Role = v }

// SetOrganizationID satisfies AuthUser.
func (u *BaseUser) SetOrganizationID(v uuid.UUID) { u.OrganizationID = v }

// SetPassword hashes plain with bcrypt at the default cost and stores the
// result in PasswordHash. The plaintext is never retained.
func (u *BaseUser) SetPassword(plain string) error {
	if plain == "" {
		return bcrypt.ErrHashTooShort
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.PasswordHash = string(hash)
	return nil
}

// CheckPassword returns true iff plain matches the stored bcrypt hash. It
// returns false for an empty or malformed stored hash rather than panicking,
// so callers can treat it as a simple predicate.
func (u *BaseUser) CheckPassword(plain string) bool {
	if u.PasswordHash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(plain)) == nil
}

// Compile-time check that *BaseUser satisfies AuthUser.
var _ AuthUser = (*BaseUser)(nil)
