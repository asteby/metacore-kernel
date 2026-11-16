package wasm

import (
	"context"

	"github.com/asteby/metacore-kernel/connectors"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

// invocationScope is the connector scope of the call being served: what the
// host put in the context (connectors.WithScope: entity and/or branch) plus the
// caller's active branch from the host branch switcher when the host set none.
// A guest never chooses it, so one entity cannot ask for another's credentials.
func invocationScope(ctx context.Context) connectors.Scope {
	scope := connectors.ScopeFrom(ctx)
	if scope.BranchID == uuid.Nil {
		if b, err := uuid.Parse(dynamic.BranchIDFromContext(ctx)); err == nil {
			scope.BranchID = b
		}
	}
	return scope
}

// scopedConnectorGet reads a connector's credentials for the invocation's org
// as seen from its scope (branch → entity → organization). With no scope, or
// over a host Store that does not keep scoped credentials, it is exactly the
// organization-level read the host has always done.
func scopedConnectorGet(ctx context.Context, inv *invocation, key string) (map[string]string, error) {
	return inv.connectors.GetScoped(ctx, inv.orgID, key, invocationScope(ctx))
}
