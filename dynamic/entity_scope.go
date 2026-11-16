package dynamic

import "context"

// Entity scope (RFC 0001 — Entidad y alcance multi-tenant).
//
// An organization may be split into ENTITIES (legal entities / razones
// sociales). An entity owns a set of branches, and a record that carries a
// branch_id belongs to the entity of that branch — the entity is derived, never
// duplicated onto every table. Documents of two entities must never be merged:
// a read made on behalf of entity A must not see rows of entity B, even though
// both live in the same organization (the hard tenant boundary stays
// organization_id; this is a second, softer wall inside it).
//
// The kernel does not know what an entity is in business terms: the embedder
// (Ops, through the `locations` addon) resolves which branches belong to the
// acting entity and hands the kernel a set of branch ids. These helpers only
// carry that decision down the call path, next to the branch and the market.

// EntityScope is the resolved visibility of one call inside an organization.
//
// The zero value (Restricted=false) means "no entity wall": the call sees the
// whole organization exactly as before the entity existed. That is what every
// call gets unless the embedder sets a scope, so adopting this type changes
// nothing by itself.
type EntityScope struct {
	// EntityID is the acting entity ("" when the call is not entity-scoped).
	EntityID string `json:"entity_id,omitempty"`
	// BranchIDs is the closed set of branches the call may see: every branch
	// of the entity, intersected by the embedder with the caller's own branch
	// membership. Meaningful only when Restricted is true.
	BranchIDs []string `json:"branch_ids,omitempty"`
	// IncludeUnassigned admits rows with a NULL branch_id. Such a row belongs
	// to the organization's DEFAULT entity (RFC §3.3), so the embedder sets
	// this only when the acting entity is the default one. Meaningful only
	// when Restricted is true.
	IncludeUnassigned bool `json:"include_unassigned,omitempty"`
	// Restricted turns the wall on. false = organization-wide (legacy).
	Restricted bool `json:"restricted,omitempty"`
	// Unresolved marks a scope the embedder FAILED to compute (resolver
	// outage). In enforce mode a branch-bearing read then fails closed instead
	// of falling open to the whole organization.
	Unresolved bool `json:"unresolved,omitempty"`
}

// Allows reports whether a row with the given branch id (uuid text, "" or nil
// for no branch) is visible inside the scope. An unrestricted scope allows
// everything; an unresolved one allows nothing that carries a branch.
func (s EntityScope) Allows(branchID string) bool {
	if s.Unresolved {
		return branchID == "" && s.IncludeUnassigned
	}
	if !s.Restricted {
		return true
	}
	if branchID == "" {
		return s.IncludeUnassigned
	}
	for _, b := range s.BranchIDs {
		if b == branchID {
			return true
		}
	}
	return false
}

type entityIDKey struct{}
type entityScopeKey struct{}

// WithEntityID returns a child context naming the entity the call acts for
// (the X-Entity-ID the host resolved). An empty id returns ctx unchanged.
// It is a REQUEST for a scope, not the scope itself: the embedder's resolver
// turns it into an EntityScope (see runtime/wasm EntityScopeResolver).
func WithEntityID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, entityIDKey{}, id)
}

// EntityIDFromContext extracts the id stored by WithEntityID, "" when none.
func EntityIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(entityIDKey{}).(string)
	return v
}

// WithEntityScope returns a child context carrying the resolved scope.
func WithEntityScope(ctx context.Context, s EntityScope) context.Context {
	return context.WithValue(ctx, entityScopeKey{}, s)
}

// EntityScopeFromContext extracts the scope stored by WithEntityScope. ok is
// false when none was set, which callers must treat as "no entity wall".
func EntityScopeFromContext(ctx context.Context) (EntityScope, bool) {
	v, ok := ctx.Value(entityScopeKey{}).(EntityScope)
	return v, ok
}
