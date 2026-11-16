package wasm

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/runtime/entityscope"
)

// Entity wall for guest data access (RFC 0001 — Entidad y alcance).
//
// An organization can be split into entities (razones sociales). Two entities of
// one organization must not read each other's documents — sales, invoices —
// even though both live under the same organization_id. The WASM host is where
// that has to hold: a guest asks for a table and filters, and nothing but the
// host stands between it and every row of the organization.
//
// The kernel does not know which branches belong to which entity. The embedder
// (Ops, through the `locations` addon) does, and supplies it through
// EntityScopeResolver. The kernel's job is the plumbing:
//
//  1. at the start of every invocation, derive the call's dynamic.EntityScope
//     from the entity / branch the context carries (the HTTP middleware sets
//     them from X-Entity-ID / X-Branch-ID; the event dispatcher sets the branch
//     of the event it replays, dispatch/ledger.go, so a handler of an event of
//     entity B runs inside B's wall);
//  2. data_query narrows its reads to the scope's branches (entityscope.Clause);
//  3. data_mutate refuses a write that names a branch of another entity.
//
// All of it is behind WithEntityScope's mode and defaults to off: a host that
// never calls it behaves exactly as before. Roll out as off → log → enforce.
// In log mode nothing is filtered or refused; every read that WOULD have been
// narrowed and every write that WOULD have been refused is logged, so the wall
// can be observed on real traffic before it can break anything.
//
// Scope of the wall (documented policy, not an accident):
//   - Only tables with a `branch_id` column are narrowed; a table without one is
//     scoped by organization alone. A model that needs entity isolation carries
//     the branch (RFC §3.3).
//   - Rows with a NULL branch_id belong to the organization's default entity
//     (dynamic.EntityScope.IncludeUnassigned).
//   - Writes: a create/update that NAMES a branch outside the scope is refused.
//     Updating or deleting an existing row of another entity by id is still
//     governed by the HTTP layer's per-record check (Ops branch scope); closing
//     it inside the host is tracked as a follow-up in the RFC.

// EntityScopeResolver turns the entity / branch a call carries into the scope
// the host enforces. It is the only seam: the wasm package knows nothing about
// how entities are stored.
//
// entityID is what the caller asked for (X-Entity-ID, "" if none); branchID is
// the active branch ("" if none). The resolver returns the closed set of
// branches the call may see (the entity's branches, intersected with the user's
// own branch membership), and whether unassigned rows are visible (true only for
// the organization's default entity). When entityID is empty and the branch
// belongs to an entity, the resolver derives the entity from the branch.
//
// Resolvers run on every invocation of a scoped call: cache per (org, entity,
// branch). A resolver error never fails the invocation; see withEntityScope.
type EntityScopeResolver interface {
	ResolveEntityScope(ctx context.Context, orgID uuid.UUID, entityID, branchID string) (dynamic.EntityScope, error)
}

// WithEntityScope enables the entity wall. mode is off (default), log or
// enforce; a nil resolver or mode off disables it entirely. See the file doc.
func (h *Host) WithEntityScope(r EntityScopeResolver, mode entityscope.Mode) *Host {
	h.entityResolver = r
	h.entityMode = mode
	return h
}

// withEntityScope stamps the call's resolved scope on ctx. It is a no-op when:
// the wall is off, there is no resolver or tenant (legacy callers), the caller
// already set a scope (an embedder that resolved it upstream wins), or the call
// carries neither an entity nor a branch (nothing to derive from: the whole
// organization, exactly as before).
//
// A resolver failure must not take guests down, and must not silently widen the
// wall either: the call gets an Unresolved scope, which in enforce mode makes a
// branch-bearing read fail closed (see entityscope.Clause) and in log mode is
// just counted.
func (h *Host) withEntityScope(ctx context.Context, orgID uuid.UUID) context.Context {
	if h.entityMode == entityscope.Off || h.entityMode == "" || h.entityResolver == nil || orgID == uuid.Nil {
		return ctx
	}
	if _, ok := dynamic.EntityScopeFromContext(ctx); ok {
		return ctx
	}
	entityID := dynamic.EntityIDFromContext(ctx)
	branchID := dynamic.BranchIDFromContext(ctx)
	if entityID == "" && branchID == "" {
		return ctx
	}
	scope, err := h.entityResolver.ResolveEntityScope(ctx, orgID, entityID, branchID)
	if err != nil {
		slog.Warn("wasm.entity_scope_unresolved",
			"org_id", orgID.String(), "entity_id", entityID, "branch_id", branchID, "err", err.Error())
		scope = dynamic.EntityScope{EntityID: entityID, Unresolved: true}
	}
	return dynamic.WithEntityScope(ctx, scope)
}

// entityScopeFor returns the scope data_query / data_mutate must honour, and
// whether the wall is active for this call at all.
func (inv *invocation) entityScopeFor(ctx context.Context) (dynamic.EntityScope, bool) {
	if inv == nil || inv.entityMode == entityscope.Off || inv.entityMode == "" {
		return dynamic.EntityScope{}, false
	}
	return dynamic.EntityScopeFromContext(ctx)
}

// checkEntityWrite is the data_mutate guard: a write naming a branch outside the
// scope is refused in enforce mode and only logged in log mode. It returns the
// envelope error code and the error, or ("", nil) to proceed.
func checkEntityWrite(ctx context.Context, inv *invocation, addonKey, table string, orgID uuid.UUID, data map[string]any) (string, error) {
	scope, on := inv.entityScopeFor(ctx)
	if !on {
		return "", nil
	}
	if entityscope.WriteViolates(scope, data) {
		slog.Warn("wasm.entity_scope_write_violation",
			"mode", string(inv.entityMode), "addon_key", addonKey, "org_id", orgID.String(),
			"entity_id", scope.EntityID, "table", table)
	}
	if err := entityscope.CheckWrite(scope, inv.entityMode, data); err != nil {
		return "forbidden", err
	}
	return "", nil
}
