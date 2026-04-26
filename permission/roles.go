package permission

import (
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// Role is the typed form of a user role string. modelbase.BaseUser stores
// role as a plain string (for schema stability) — this type exists so
// signatures in the permission package document intent and so apps can
// range-check values at compile time.
type Role string

// Canonical role names recognised by the kernel's default policies. They
// mirror the constants in modelbase/user.go to avoid cross-package drift.
// Apps MAY define additional roles; the kernel treats them as ordinary roles
// whose capabilities come from the store.
const (
	RoleOwner Role = Role(modelbase.RoleOwner)
	RoleAdmin Role = Role(modelbase.RoleAdmin)
	RoleAgent Role = Role(modelbase.RoleAgent)
)

// String returns the underlying role name.
func (r Role) String() string { return string(r) }

// Normalize trims whitespace and lowercases a role string. Used by the store
// lookups so casing differences between DB rows and config do not cause
// spurious "unknown role" errors.
func NormalizeRole(r string) Role {
	return Role(strings.ToLower(strings.TrimSpace(r)))
}

// DefaultSuperRoles is the out-of-the-box list of roles that bypass every
// capability check. RoleOwner is included to match the behaviour of typical
// host permission services (which hardcode owner/admin/superadmin as
// unconditional wildcards).
//
// Apps that want "admin also bypasses" pass their own via Config.SuperRoles.
func DefaultSuperRoles() []Role {
	return []Role{RoleOwner}
}
