# Entity scope — the wall between two entities of one organization

Status: implemented behind a flag, default **off** (kernel PR for RFC 0001 §3.4 / §3.5 / §5).
RFC: `docs/rfc/0001-entidad-y-alcance.md` (PR #595).

An organization (the tenant, `organization_id`) may be split into **entities**
(razones sociales; RFC, series, PAC of their own). Two entities of one group
must never merge sales or invoices, even though they share the organization:

```
organization  ──hard wall──  organization          (existing: organization_id)
   └ entity A ──soft wall──  entity B               (new: this document)
        └ branch 1, branch 2     └ branch 3
```

The kernel does not know what an entity is in business terms. The embedder
(Ops, through the `locations` addon) resolves **which branches belong to the
acting entity**; the kernel carries that decision down the call path and
enforces it where a guest could otherwise read the whole organization: the WASM
host.

## What was added

| Piece | Where | Role |
|---|---|---|
| `dynamic.EntityScope`, `WithEntityID`, `WithEntityScope` | `dynamic/entity_scope.go` | Context carriers next to branch and market. Zero value = no wall. |
| `entityscope` package | `runtime/entityscope` | Pure, DB-free: `Mode`, `Clause` (SQL narrowing), `OutOfScope` (log probe), `CheckWrite`. |
| `wasm.EntityScopeResolver`, `Host.WithEntityScope(resolver, mode)` | `runtime/wasm/entity_scope.go` | The only seam: the embedder turns `{entity, branch}` into a scope. |
| `data_query` wall | `runtime/wasm/dataquery_records.go` | Enforce: `branch_id IN (...)` after the org predicate. Log: counts what would be hidden. |
| `data_mutate` guard | `runtime/wasm/datamutate.go` | Create/update that **names** a branch outside the scope → `forbidden`. |
| Entity settings layer | `manifest`, `settings`, migration `0011` | `default < org < entity < branch < market`; `scope: entity`; a `scope: branch` key inherits its entity's value. |

## Contract

```go
type EntityScope struct {
    EntityID          string   // acting entity ("" = not entity-scoped)
    BranchIDs         []string // closed set the call may see
    IncludeUnassigned bool     // rows with branch_id NULL (default entity only)
    Restricted        bool     // false = whole organization (legacy)
    Unresolved        bool     // resolver failed: fail CLOSED in enforce
}

type EntityScopeResolver interface {
    ResolveEntityScope(ctx, orgID uuid.UUID, entityID, branchID string) (dynamic.EntityScope, error)
}
```

Wiring (Ops): `host.WithEntityScope(locationsResolver, entityscope.ParseMode(os.Getenv("ENTITY_SCOPE")))`.
`ParseMode` treats any unknown value as `off`: a typo never switches the wall on or off by surprise.

### Where the scope comes from

At the start of every `Invoke*` that carries an org, the host derives the scope
from the context:

1. A scope already on the context (an embedder that resolved it upstream) wins.
2. Otherwise, from `dynamic.EntityIDFromContext` (HTTP: `X-Entity-ID`) and/or
   `dynamic.BranchIDFromContext` (HTTP: `X-Branch-ID`), via the resolver. With a
   branch only, the resolver derives the entity from it.
3. With neither, nothing is derived: the whole organization, exactly as before.

**Events.** The dispatcher already stamps the event's branch on the context
(`dispatch/ledger.go`, `dynamic/outbox.go`), so a handler replaying an event of
entity B runs inside B's wall and cannot read A's documents.

**Settings.** `settings.WasmResolver` reads the entity from the same context, so
`env_get` returns the entity-level value with no change to guests.

## Policy (documented, not accidental)

- **Only tables with a `branch_id` column are narrowed.** The entity of such a row
  is the entity of its branch (RFC §3.3); it is never duplicated. A table without
  `branch_id` is scoped by organization alone. A model that needs entity isolation
  must carry the branch.
- **`branch_id IS NULL` belongs to the organization's default entity.** Visible
  only when the scope sets `IncludeUnassigned`.
- **Writes:** a create/update whose payload **names** a `branch_id` outside the
  scope is refused (`forbidden`). A payload that names none is fine: the host
  stamps the caller's own active branch, which the scope contains.
- **Unresolved scope (resolver outage):** in enforce, a branch-bearing read sees
  nothing (never everything); in log, it is only counted. The invocation itself
  never fails because of the resolver.
- **Aggregates and projections** (no `branch_id` in the result) are not judged by
  the log probe; the enforce predicate still applies to the underlying rows.
- **Not closed yet (tracked in the RFC):** updating/deleting an existing row of
  another entity *by id* inside the WASM host. The HTTP layer's per-record branch
  check (Ops) still governs it; closing it in the host needs a pre-read scope
  check in `data_mutate`.

## Rollout: off → log → enforce

1. **off** (default). Nothing changes. Merging this is safe.
2. **log**. Nothing is filtered or refused. Every read that *would* have been
   narrowed logs `wasm.entity_scope_would_filter` (addon, org, entity, table,
   `rows_read`, `rows_out_of_scope`); every refused-to-be write logs
   `wasm.entity_scope_write_violation`; resolver failures log
   `wasm.entity_scope_unresolved`. Run for a week of real traffic and read the
   logs: a non-zero `rows_out_of_scope` is either a leak you just found or a
   report that must become group-level analytics (see below).
3. **enforce**. The wall is exact.

Roll out per environment, 7Leguas first. Roll back by setting the mode to `off`;
no data migration is involved (the only schema change is the settings CHECK).

## Group-level statistics are not a leak

The wall protects **transactional documents** (sales, invoices, payments). The
business also needs read-only **group aggregates** ("sales of the whole group").
Those must not use the transactional read path with the wall off; they go through
a dedicated aggregate capability that returns counts/sums and never rows of
another entity. That is a separate piece (RFC §3.7 analytics) and is not weakened
by this one.

## Performance

`branch_id IN (...)` over the entity's branches (a handful per entity; the clause
refuses more than 5 000 rather than truncating). Postgres uses an index on
`branch_id`; the existing org predicate stays first. Guidance (RFC §3.3): do **not**
denormalize `entity_id` onto tables up front. If a per-entity listing exceeds
p95 > 300 ms at the real data size (≈10⁶ rows in the largest table), add
`(organization_id, branch_id)` to that table first; only then consider a stored
`entity_id` maintained by trigger, with a reversible backfill.

## Adoption by addon

Addons whose manifests already declare a `branch_id` column get the wall for free
when the mode is enforce: `inventory`, `pos`, `locations`, `warehouse`,
`requisitions`, `expenses`, `customers` (sales orders), `cash-recycler`,
`workstations`, `sales_teams`, `quotes`, `payment-terminals`, `mercadopago`,
`hr-lite`.

Gaps to close before enforcing on 7Leguas ecommerce (RFC phase F4): the checkout
and payment models of the ecommerce path (`storefront_cart` checkout sessions,
payment attempts, `mp_payments`) do **not** carry a `branch_id` today, so the wall
cannot see them; they need the branch (and an immutable entity snapshot on issued
fiscal documents). Catalog models (`products`) are shared by design.

Settings: an addon opts a key into the entity layer with `scope: entity` (or
`scope: branch` to let an entity set a default for all its branches). Existing
manifests are unchanged.

## Tests (RFC §5 → code)

| RFC §5 isolation row | Test |
|---|---|
| WASM read with A's context returns no B rows | `runtime/wasm/entity_scope_dataquery_test.go` (exact predicate, order, args, fail-closed, off/log inert) |
| A guest filter cannot widen the wall | `TestDataQueryWall_EntityPredicateSitsBetweenOrgScopeAndGuestFilters` |
| Create with B's branch while in A → refused | `runtime/entityscope` `TestCheckWrite`, `runtime/wasm` `TestCheckEntityWrite` |
| Org without entities: same behaviour | `TestDataQueryWall_OffAndMissingScopeLeaveTheQueryExactlyAsBefore` |
| Resolver outage never widens the wall | `TestWithEntityScope_ResolverFailureNeverWidensTheWall` |
| Event of entity B cannot read A | `TestWithEntityScope_DerivesFromBranchAndEntity` (branch-only derivation, the replay path) |
| Entity settings precedence and write rules | `settings/entity_test.go`, `manifest/settings_entity_test.go` |

The SQL semantics of `IN` are Postgres'; a database-backed isolation test (two
entities, real rows) belongs with the `locations` addon PR (RFC F1/F2), which owns
the entity tables.
