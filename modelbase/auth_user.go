package modelbase

import "github.com/google/uuid"

// AuthUser is the contract the auth and permission modules depend on. It is
// intentionally written in terms of getters + setters (rather than struct
// field access) so apps can supply richer User shapes via embedding of
// BaseUser — or a wholly bespoke implementation — without breaking the kernel.
//
// *BaseUser implements AuthUser out of the box; any struct that embeds
// BaseUser therefore satisfies it automatically.
//
// The setter methods are used by auth.Register during new-account creation;
// read-only callers (login, permission checks) never mutate the user.
type AuthUser interface {
	// Identity + tenant scoping.
	GetID() uuid.UUID
	GetOrganizationID() uuid.UUID
	GetEmail() string
	GetRole() string
	// Credential material. Stored as bcrypt hash, never as plaintext.
	GetPasswordHash() string

	// Setters used by the register flow.
	SetEmail(string)
	SetName(string)
	SetPasswordHash(string)
	SetRole(string)
	SetOrganizationID(uuid.UUID)
}

// AuthOrg is the minimal contract for organisation/tenant models the auth
// module manipulates during registration. Apps that embed BaseOrganization
// satisfy it automatically.
type AuthOrg interface {
	GetID() uuid.UUID
	GetName() string
	SetName(string)
}
