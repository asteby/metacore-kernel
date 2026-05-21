package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// JWTClaimsProvider is the AuthUserProvider materialised by
// JWTClaimsExtractor. The fields are exported so apps with their own claims
// shape can construct one directly without going through the extractor (e.g.
// in unit tests of business handlers).
type JWTClaimsProvider struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// RoleKeys holds the claim's role(s). The default *auth.Claims shape
	// stores a single role string; richer claim sets (Plan, Audience, …) can
	// supply multi-role slices via JWTClaimsConfig.RolesFromClaims.
	RoleKeys []string
	// Capabilities is the optional pre-resolved capability list lifted from
	// the JWT. When populated, the provider satisfies PermissionChecker and
	// the kernel skips the permission.Service round-trip.
	Capabilities []string
}

// UserID implements AuthUserProvider.
func (p JWTClaimsProvider) UserID() uuid.UUID { return p.ID }

// OrganizationID implements AuthUserProvider.
func (p JWTClaimsProvider) OrganizationID() uuid.UUID { return p.OrgID }

// Roles implements AuthUserProvider.
func (p JWTClaimsProvider) Roles() []string { return p.RoleKeys }

// HasPermission implements PermissionChecker. Returns false when no
// capabilities are attached so the kernel falls back to permission.Service.
// Wildcard "*" matches every capability — same semantics as
// permission.Capability.Matches.
func (p JWTClaimsProvider) HasPermission(key string) bool {
	for _, c := range p.Capabilities {
		if c == "*" || c == key {
			return true
		}
	}
	return false
}

// ClaimsAccessor is the minimal contract JWTClaimsExtractor needs in order to
// pull values out of an arbitrary claims struct. Apps with custom claim
// shapes implement it (most often by embedding *auth.Claims, which already
// satisfies the methods via its struct fields). The interface intentionally
// uses Go-level types — uuid.UUID, []string — so the extractor does not have
// to know about jwt.RegisteredClaims.
type ClaimsAccessor interface {
	GetUserID() uuid.UUID
	GetOrganizationID() uuid.UUID
	GetRoleKeys() []string
}

// CapabilityClaims is the optional extension a claims struct implements when
// the JWT carries a pre-resolved capability list. When the accessor satisfies
// it, the resulting provider implements PermissionChecker and the kernel
// skips its permission.Service call.
type CapabilityClaims interface {
	GetCapabilities() []string
}

// JWTClaimsConfig customises JWTClaimsExtractor. ClaimsKey is required; the
// rest let apps lift values out of a non-standard claims shape without
// implementing ClaimsAccessor on their own type.
type JWTClaimsConfig struct {
	// ClaimsKey is the Fiber locals key holding the parsed claims object.
	// Defaults to auth.LocalClaims ("auth_claims") when empty — the value
	// written by auth.Middleware.
	ClaimsKey string

	// IDFromClaims overrides how the user id is read. nil falls back to
	// ClaimsAccessor.GetUserID.
	IDFromClaims func(claims any) uuid.UUID

	// OrgFromClaims overrides how the organization id is read.
	OrgFromClaims func(claims any) uuid.UUID

	// RolesFromClaims overrides how the role keys are read. Useful for apps
	// with multi-role JWTs (e.g. claims.Roles []string instead of single
	// claims.Role string).
	RolesFromClaims func(claims any) []string

	// CapabilitiesFromClaims optionally lifts a pre-resolved capability list
	// out of the claims. When it returns a non-empty slice, the provider
	// satisfies PermissionChecker.
	CapabilitiesFromClaims func(claims any) []string
}

const defaultClaimsKey = "auth_claims"

// JWTClaimsExtractor returns an AuthUserExtractor that reads the parsed JWT
// claims out of Fiber locals (the value written by auth.Middleware) and
// projects them onto an AuthUserProvider. Apps with their own claims struct
// supply lifter functions via cfg.
//
// Wiring with the default auth.Claims is a one-liner:
//
//	extractor := adapters.JWTClaimsExtractor(adapters.JWTClaimsConfig{})
//
// Wiring with a custom claims shape carrying multi-role + capabilities:
//
//	extractor := adapters.JWTClaimsExtractor(adapters.JWTClaimsConfig{
//	    ClaimsKey:              "marketplace_claims",
//	    IDFromClaims:           func(c any) uuid.UUID { return c.(*MyClaims).UserID },
//	    OrgFromClaims:          func(c any) uuid.UUID { return c.(*MyClaims).OrgID },
//	    RolesFromClaims:        func(c any) []string  { return c.(*MyClaims).Roles },
//	    CapabilitiesFromClaims: func(c any) []string  { return c.(*MyClaims).Caps },
//	})
func JWTClaimsExtractor(cfg JWTClaimsConfig) AuthUserExtractor {
	key := cfg.ClaimsKey
	if key == "" {
		key = defaultClaimsKey
	}

	return func(ctx context.Context) (AuthUserProvider, error) {
		c := FiberCtxFrom(ctx)
		if c == nil {
			return nil, nil
		}

		raw := c.Locals(key)
		if raw == nil {
			return nil, nil
		}

		var (
			id    uuid.UUID
			orgID uuid.UUID
			roles []string
			caps  []string
		)

		// Lifter functions take precedence; fall back to ClaimsAccessor when
		// the app's claims type satisfies it.
		if cfg.IDFromClaims != nil {
			id = cfg.IDFromClaims(raw)
		} else if a, ok := raw.(ClaimsAccessor); ok {
			id = a.GetUserID()
		} else {
			return nil, fmt.Errorf("adapters: claims (%T) does not implement ClaimsAccessor and no IDFromClaims supplied", raw)
		}
		if id == uuid.Nil {
			return nil, nil
		}

		if cfg.OrgFromClaims != nil {
			orgID = cfg.OrgFromClaims(raw)
		} else if a, ok := raw.(ClaimsAccessor); ok {
			orgID = a.GetOrganizationID()
		}

		if cfg.RolesFromClaims != nil {
			roles = cfg.RolesFromClaims(raw)
		} else if a, ok := raw.(ClaimsAccessor); ok {
			roles = a.GetRoleKeys()
		}

		if cfg.CapabilitiesFromClaims != nil {
			caps = cfg.CapabilitiesFromClaims(raw)
		} else if a, ok := raw.(CapabilityClaims); ok {
			caps = a.GetCapabilities()
		}

		return JWTClaimsProvider{
			ID:           id,
			OrgID:        orgID,
			RoleKeys:     roles,
			Capabilities: caps,
		}, nil
	}
}
