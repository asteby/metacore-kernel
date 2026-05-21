package adapters

import (
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// providerAsModelbase adapts a narrow AuthUserProvider back to the fat
// modelbase.AuthUser interface. This is the bridge the kernel uses to keep
// existing dynamic.Service / permission.Service / hook signatures intact
// while still letting apps wire their own provider shape at the HTTP
// boundary.
//
// The setter half of modelbase.AuthUser is intentionally a no-op: auth flows
// (Register, password change, …) always run against a real *BaseUser the app
// loaded from the database. Provider-wrapped users only reach the read paths.
//
// GetPasswordHash returns the empty string — never use providerAsModelbase in
// a code path that authenticates a credential. The kernel checkPerm /
// tenant-scope paths never read it, so the zero value is safe.
type providerAsModelbase struct {
	p AuthUserProvider
}

// GetID implements modelbase.AuthUser.
func (a providerAsModelbase) GetID() uuid.UUID { return a.p.UserID() }

// GetOrganizationID implements modelbase.AuthUser.
func (a providerAsModelbase) GetOrganizationID() uuid.UUID { return a.p.OrganizationID() }

// GetEmail implements modelbase.AuthUser. Provider-based principals do not
// carry the email, so we return the empty string. Apps that need the email
// at a hook layer should keep using modelbase.AuthUser end-to-end (or layer
// their own provider type with an email getter).
func (a providerAsModelbase) GetEmail() string { return "" }

// GetRole implements modelbase.AuthUser. Returns the first role from the
// provider (the kernel's permission.Service is single-role for the
// SuperRole short-circuit and store lookup); empty when no roles.
func (a providerAsModelbase) GetRole() string {
	roles := a.p.Roles()
	if len(roles) == 0 {
		return ""
	}
	return roles[0]
}

// GetPasswordHash implements modelbase.AuthUser. Never populated for
// provider-wrapped users — credential checks must use the original
// *BaseUser.
func (a providerAsModelbase) GetPasswordHash() string { return "" }

// SetEmail implements modelbase.AuthUser as a no-op.
func (a providerAsModelbase) SetEmail(string) {}

// SetName implements modelbase.AuthUser as a no-op.
func (a providerAsModelbase) SetName(string) {}

// SetPasswordHash implements modelbase.AuthUser as a no-op.
func (a providerAsModelbase) SetPasswordHash(string) {}

// SetRole implements modelbase.AuthUser as a no-op.
func (a providerAsModelbase) SetRole(string) {}

// SetOrganizationID implements modelbase.AuthUser as a no-op.
func (a providerAsModelbase) SetOrganizationID(uuid.UUID) {}

// WrapAsModelbase turns an AuthUserProvider into a modelbase.AuthUser so it
// can flow through the kernel's internal pipeline (dynamic.Service,
// permission.Service, hooks) without changing those signatures.
//
// When p is itself a modelbaseProvider (i.e. the original was a
// modelbase.AuthUser) the wrapper short-circuits and returns the underlying
// instance instead of a no-op shell. This keeps the round-trip lossless for
// apps that already use modelbase.AuthUser end-to-end.
//
// Returns nil when p is nil — callers can treat the result as "no auth"
// without a separate guard.
func WrapAsModelbase(p AuthUserProvider) modelbase.AuthUser {
	if p == nil {
		return nil
	}
	if mp, ok := p.(modelbaseProvider); ok {
		return mp.u
	}
	return providerAsModelbase{p: p}
}
