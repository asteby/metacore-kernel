package connectors

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Scope says WHERE in the tenant hierarchy a connector credential is read from.
//
// The hierarchy is organization → entity (razón social) → branch. A credential
// can be stored at any of the three levels; the most specific one that exists
// for the caller wins (branch, then entity, then the organization). The kernel
// never learns what an entity is: both ids are opaque UUIDs the host fills in
// from its own model (RFC 0001, §3.5).
//
// The zero Scope is the organization level, which is exactly what Get does, so
// an addon that never sets a scope behaves as before.
type Scope struct {
	// EntityID is the legal entity the call runs for (zero = unknown / none).
	EntityID uuid.UUID
	// BranchID is the branch the call runs for (zero = unknown / none).
	BranchID uuid.UUID
}

// IsZero reports whether the scope carries neither an entity nor a branch.
func (s Scope) IsZero() bool { return s.EntityID == uuid.Nil && s.BranchID == uuid.Nil }

// String renders the scope for audit logs and error messages.
func (s Scope) String() string {
	switch {
	case s.IsZero():
		return "org"
	case s.BranchID != uuid.Nil && s.EntityID != uuid.Nil:
		return fmt.Sprintf("branch=%s entity=%s", s.BranchID, s.EntityID)
	case s.BranchID != uuid.Nil:
		return fmt.Sprintf("branch=%s", s.BranchID)
	default:
		return fmt.Sprintf("entity=%s", s.EntityID)
	}
}

type scopeCtxKey struct{}

// WithScope returns a context carrying the scope the host resolved for the
// request (typically from its branch switcher and the branch → entity link).
// The wasm host reads it when a guest calls connector_get / smtp_send, so an
// addon needs no change to read the right credentials. A zero scope leaves the
// context untouched.
func WithScope(ctx context.Context, s Scope) context.Context {
	if s.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, scopeCtxKey{}, s)
}

// ScopeFrom returns the scope set by WithScope, or the zero Scope.
func ScopeFrom(ctx context.Context) Scope {
	if ctx == nil {
		return Scope{}
	}
	s, _ := ctx.Value(scopeCtxKey{}).(Scope)
	return s
}

// ScopedStore is the optional extension of Store for hosts that keep
// credentials per entity or per branch. A Store that does not implement it keeps
// working: GetScoped falls back to Get (organization level).
//
// GetScoped resolves the most specific credential for the scope, walking
// branch → entity → organization. It returns (creds, true, nil) on a hit,
// (nil, false, nil) when nothing applies, and ErrScopedConnectorRequired
// (wrapped) when the connector is configured so that an entity may NOT inherit
// the organization's credentials and the entity has none of its own (for
// example payments: money must land in the selling entity's own account).
type ScopedStore interface {
	GetScoped(ctx context.Context, orgID uuid.UUID, connectorKey string, scope Scope) (map[string]string, bool, error)
}

// ErrScopedConnectorRequired is returned by a ScopedStore when the caller's
// entity must have a connector of its own and has none. It is actionable: the
// fix is to connect the provider for that entity, not to retry.
var ErrScopedConnectorRequired = errors.New("connectors: this entity has no connector of its own and the organization's is not shared with it")

// GetScoped resolves the credentials for (org, connectorKey) as seen from
// scope. With a zero scope, or over a Store that is not a ScopedStore, it is
// exactly Get. Errors keep the same sentinels as Get (ErrNoStore,
// ErrConnectorNotFound) plus ErrScopedConnectorRequired.
func (r *Resolver) GetScoped(ctx context.Context, orgID uuid.UUID, connectorKey string, scope Scope) (map[string]string, error) {
	if r == nil || r.store == nil {
		return nil, ErrNoStore
	}
	ss, ok := r.store.(ScopedStore)
	if scope.IsZero() || !ok {
		return r.Get(ctx, orgID, connectorKey)
	}
	creds, found, err := ss.GetScoped(ctx, orgID, connectorKey, scope)
	if err != nil {
		if errors.Is(err, ErrScopedConnectorRequired) {
			return nil, fmt.Errorf("%w: %q (%s)", err, connectorKey, scope)
		}
		return nil, fmt.Errorf("connectors: store get %q (%s): %w", connectorKey, scope, err)
	}
	if !found {
		return nil, fmt.Errorf("%w: %q (%s)", ErrConnectorNotFound, connectorKey, scope)
	}
	return creds, nil
}
