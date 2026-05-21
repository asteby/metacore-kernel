package adapters

import (
	"context"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// modelbaseProvider adapts a modelbase.AuthUser to the narrow
// AuthUserProvider contract. It holds the wrapped user directly so apps
// that already loaded a full User row pay no extra allocation per check.
type modelbaseProvider struct {
	u modelbase.AuthUser
}

// UserID implements AuthUserProvider.
func (p modelbaseProvider) UserID() uuid.UUID { return p.u.GetID() }

// OrganizationID implements AuthUserProvider.
func (p modelbaseProvider) OrganizationID() uuid.UUID { return p.u.GetOrganizationID() }

// Roles implements AuthUserProvider. modelbase.AuthUser only exposes a single
// role string (the kernel's baseline RBAC model). The slice contains one
// element — empty role yields an empty slice so callers can range without a
// guard.
func (p modelbaseProvider) Roles() []string {
	r := p.u.GetRole()
	if r == "" {
		return nil
	}
	return []string{r}
}

// Unwrap returns the underlying modelbase.AuthUser. Used by the kernel-internal
// bridge in WrapAsModelbase so a round-trip
// (modelbase.AuthUser -> Provider -> modelbase.AuthUser) returns the original
// instance instead of a synthetic wrapper.
func (p modelbaseProvider) Unwrap() modelbase.AuthUser { return p.u }

// WrapModelbaseUser turns a modelbase.AuthUser into an AuthUserProvider. Apps
// that already loaded a full User row (auth.Service.Me, ORM lookups, …) wrap
// once and pass the provider down — no extra DB hits, no field copies.
//
// Returns nil if u is nil so callers can chain
//
//	provider := adapters.WrapModelbaseUser(authService.UserFromCtx(c))
//
// and treat a nil provider as "no auth".
func WrapModelbaseUser(u modelbase.AuthUser) AuthUserProvider {
	if u == nil {
		return nil
	}
	return modelbaseProvider{u: u}
}

// ModelbaseExtractor returns an AuthUserExtractor that pulls a
// modelbase.AuthUser out of ctx via the supplied lookup function. Apps that
// already have a context-keyed user instance (e.g. from auth.Service) wire it
// like:
//
//	extractor := adapters.ModelbaseExtractor(func(ctx context.Context) modelbase.AuthUser {
//	    return ctx.Value(myAuthKey).(modelbase.AuthUser)
//	})
//
// The lookup returning nil maps to "no auth" (provider == nil, err == nil).
func ModelbaseExtractor(lookup func(ctx context.Context) modelbase.AuthUser) AuthUserExtractor {
	return func(ctx context.Context) (AuthUserProvider, error) {
		u := lookup(ctx)
		if u == nil {
			return nil, nil
		}
		return modelbaseProvider{u: u}, nil
	}
}
