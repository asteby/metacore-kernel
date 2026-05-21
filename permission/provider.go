package permission

import (
	"context"

	"github.com/asteby/metacore-kernel/auth/adapters"
)

// CheckProvider is the AuthUserProvider-shaped variant of Check. Apps wired
// to an adapters.AuthUserExtractor call this directly instead of the legacy
// modelbase.AuthUser flavour — saving a round-trip through
// adapters.WrapAsModelbase when only the read-side check is needed.
//
// Behaviour mirrors Check exactly:
//
//   - ErrNoUser when provider is nil.
//   - When the provider implements adapters.PermissionChecker (e.g.
//     JWTClaimsProvider with a populated Capabilities list), the in-claim
//     authority short-circuits before any store lookup. This is the
//     pre-resolved capabilities path.
//   - Otherwise it delegates to Check using a transient modelbase.AuthUser
//     wrapper.
func (s *Service) CheckProvider(ctx context.Context, provider adapters.AuthUserProvider, cap Capability) error {
	if provider == nil {
		return ErrNoUser
	}
	if checker, ok := provider.(adapters.PermissionChecker); ok {
		if checker.HasPermission(string(cap)) {
			return nil
		}
		// Pre-resolved capability list explicitly does not grant cap. Apps
		// can still grant via role/store by passing a provider that does
		// NOT implement PermissionChecker — surfacing the "claims are
		// authoritative" decision here matches how JWT systems are usually
		// deployed.
	}
	return s.Check(ctx, adapters.WrapAsModelbase(provider), cap)
}

// CheckProviderAny mirrors CheckAny for the provider contract.
func (s *Service) CheckProviderAny(ctx context.Context, provider adapters.AuthUserProvider, caps ...Capability) error {
	if provider == nil {
		return ErrNoUser
	}
	if checker, ok := provider.(adapters.PermissionChecker); ok {
		for _, c := range caps {
			if checker.HasPermission(string(c)) {
				return nil
			}
		}
	}
	return s.CheckAny(ctx, adapters.WrapAsModelbase(provider), caps...)
}

// CheckProviderAll mirrors CheckAll for the provider contract.
func (s *Service) CheckProviderAll(ctx context.Context, provider adapters.AuthUserProvider, caps ...Capability) error {
	if provider == nil {
		return ErrNoUser
	}
	if checker, ok := provider.(adapters.PermissionChecker); ok {
		allMatch := true
		for _, c := range caps {
			if !checker.HasPermission(string(c)) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return nil
		}
	}
	return s.CheckAll(ctx, adapters.WrapAsModelbase(provider), caps...)
}
