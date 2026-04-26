# Permissions

Capability-based authorization — for **users** at the HTTP edge and for
**addons** at every privileged kernel call. This document covers both
systems, when they fire, and how to wire them.

For the dynamic CRUD framework that consumes these gates, see
[`dynamic-system.md`](dynamic-system.md).

---

## Table of contents

- [Two systems, one principle](#two-systems-one-principle)
- [User-level capabilities](#user-level-capabilities)
  - [Capability shape](#capability-shape)
  - [Service](#service)
  - [Stores](#stores)
  - [Roles and super-roles](#roles-and-super-roles)
  - [Fiber gate middleware](#fiber-gate-middleware)
- [Addon-level capabilities](#addon-level-capabilities)
  - [Declaring in manifest](#declaring-in-manifest)
  - [Compiled policy](#compiled-policy)
  - [Enforcer modes](#enforcer-modes)
  - [Walkthrough of a check](#walkthrough-of-a-check)
- [Best practices](#best-practices)
- [See also](#see-also)

---

## Two systems, one principle

| System                 | Subject       | Question answered                                          | Lives in |
| ---------------------- | ------------- | ---------------------------------------------------------- | -------- |
| `permission.Service`   | Authenticated user (HTTP) | May this user perform action X on resource Y?  | [`permission/`](../permission/) |
| `security.Enforcer`    | Installed addon            | May this addon DB-write / HTTP-fetch / emit?    | [`security/`](../security/)     |

They are independent. A request can pass the user gate, the addon gate, or
both. The CRUD handler runs the user gate per request; the addon gate fires
inside the kernel's host imports when an addon attempts a privileged call.

The shared design principle: **least privilege, declarative grants,
runtime enforcement, audit-friendly violations**.

## User-level capabilities

### Capability shape

A capability is a string in `<resource>.<action>` form. The constructor
trims whitespace and lowercases the resource segment so casing differences
between Go code and DB rows never matter.

```go
permission.Cap("Tickets", "Create")  // → permission.Capability("tickets.Create")
permission.Cap("invoices", "approve") // → permission.Capability("invoices.approve")
```

The wildcard `"*"` is reserved: any role or user holding `*` matches every
check. See [`permission/capability.go`](../permission/capability.go).

The dynamic CRUD service synthesises the capability for every request:

| HTTP                                | Capability checked  |
| ----------------------------------- | ------------------- |
| `GET    /api/dynamic/<model>`       | `<model>.read`      |
| `GET    /api/dynamic/<model>/:id`   | `<model>.read`      |
| `POST   /api/dynamic/<model>`       | `<model>.create`    |
| `PUT    /api/dynamic/<model>/:id`   | `<model>.update`    |
| `DELETE /api/dynamic/<model>/:id`   | `<model>.delete`    |

The synthesis is in
[`dynamic/service.go:checkPerm`](../dynamic/service.go).

Common action verbs declared as constants — apps are free to invent more:

| Constant         | String   |
| ---------------- | -------- |
| `CapCreate`      | `create` |
| `CapRead`        | `read`   |
| `CapUpdate`      | `update` |
| `CapDelete`      | `delete` |
| `CapList`        | `list`   |
| `CapExport`      | `export` |
| `CapImport`      | `import` |

### Service

`permission.Service` ([`permission/service.go`](../permission/service.go))
is the framework-agnostic check engine. Three call shapes:

```go
err := svc.Check(ctx, user, permission.Cap("tickets", "create"))
err := svc.CheckAny(ctx, user, capA, capB)   // ≥1 of caps
err := svc.CheckAll(ctx, user, capA, capB)   // every cap
```

All three return `nil` on success and `permission.ErrPermissionDenied`
(wrapped) on failure. `ErrNoUser` is returned when `user` is `nil`.

The service composes:

- a `PermissionStore` (where grants live),
- a `capCache` keyed by user id with TTL `Config.CacheTTL`
  (default 5 min, `-1` disables),
- a set of super-roles that bypass every check.

Capability resolution — `GetUserCapabilities(ctx, user)` — combines:

1. Role grants from the store, deduplicated.
2. Per-user grants from the store, additive.

The result is cached per user. `InvalidateUser(uid)` and `InvalidateAll()`
clear the cache after a grant change.

### Stores

`permission.PermissionStore` is the stable contract:

```go
type PermissionStore interface {
    GetRolePermissions(ctx context.Context, role Role) ([]Capability, error)
    GetUserPermissions(ctx context.Context, userID uuid.UUID) ([]Capability, error)
}
```

Two implementations ship in [`permission/store.go`](../permission/store.go):

| Store           | Use when                                         | Persistence                              |
| --------------- | ------------------------------------------------ | ---------------------------------------- |
| `InMemoryStore` | Tests, or apps with a fully static role policy   | None (declared at boot)                  |
| `GormStore`     | Production default                               | `permission_role_grants`, `permission_user_grants` |

`GormStore` exposes idempotent helpers for bootstrapping:

```go
store, err := permission.NewGormStore(db)
_ = store.GrantRole(ctx, permission.RoleAdmin, permission.Cap("tickets", "create"))
_ = store.GrantRole(ctx, permission.RoleAdmin, permission.Cap("tickets", "update"))
_ = store.GrantUser(ctx, alice.ID, permission.Cap("tickets", "delete"))
```

Apps with custom requirements (Redis cache, branch-scoped grants, addon
policy engines) implement `PermissionStore` themselves.

### Roles and super-roles

Roles are typed strings ([`permission/roles.go`](../permission/roles.go)).
The kernel ships three canonical names — apps may freely add their own:

| Constant     | String  |
| ------------ | ------- |
| `RoleOwner`  | `owner` |
| `RoleAdmin`  | `admin` |
| `RoleAgent`  | `agent` |

`DefaultSuperRoles()` returns `[]Role{RoleOwner}` — owners bypass every
check (a single synthetic `Wildcard` capability is returned for them).
Override with `Config.SuperRoles`:

```go
svc := permission.New(permission.Config{
    Store:      store,
    SuperRoles: []permission.Role{permission.RoleOwner, permission.RoleAdmin},
})
```

### Fiber gate middleware

Plug a capability check anywhere in the route tree
([`permission/middleware.go`](../permission/middleware.go)):

```go
api.Post("/tickets/:id/escalate",
    permSvc.Gate(userLookup, permission.Cap("tickets", "escalate")),
    ticketHandler.Escalate)
```

`Gate` is the single-capability shortcut. `GateWith` accepts a
`GateConfig` for multi-cap calls and custom error responders:

```go
api.Post("/billing/refund",
    permSvc.GateWith(userLookup, permission.GateConfig{
        Mode: permission.ModeAny,        // OR semantics
        OnDenied: func(c *fiber.Ctx, err error) error {
            return c.Status(403).JSON(fiber.Map{"error": "billing access required"})
        },
    },
    permission.Cap("billing", "refund"),
    permission.Cap("billing", "admin"),
    ),
    billingHandler.Refund,
)
```

`UserLookup` is `func(*fiber.Ctx) modelbase.AuthUser`. Returning nil
yields `401`. Failing the cap check yields `403`.

For dynamic CRUD specifically, the gate is integrated automatically: as
long as `host.AppConfig.PermissionStore` is non-nil, every CRUD request
calls `Service.Check` before touching the database.

## Addon-level capabilities

### Declaring in manifest

Addons ship a `capabilities[]` block in `manifest.json`. Each entry has a
`kind`, a `target`, and an optional `reason`. The marketplace prompts the
admin for approval before installation.

```json
{
  "key": "tickets",
  "capabilities": [
    { "kind": "db:read",         "target": "addon_tickets.*",     "reason": "Read own tickets" },
    { "kind": "db:write",        "target": "addon_tickets.*",     "reason": "Create and edit tickets" },
    { "kind": "http:fetch",      "target": "api.stripe.com",      "reason": "Refund payments" },
    { "kind": "event:emit",      "target": "ticket.created",      "reason": "Notify other addons" },
    { "kind": "event:subscribe", "target": "invoice.stamped",     "reason": "Auto-link invoices" }
  ]
}
```

Supported `kind` values are exhaustive — the enforcer rejects any other:

| Kind               | Target shape                              | Enforced where                                |
| ------------------ | ----------------------------------------- | --------------------------------------------- |
| `db:read`          | Model glob: `orders`, `addon_tickets.*`   | Host imports for read paths                   |
| `db:write`         | Model glob (same as `db:read`)            | Host imports for create/update/delete         |
| `http:fetch`       | Host with at least one dot, optional `*.` | Outbound HTTP from inside the WASM sandbox    |
| `event:emit`       | Event name or `prefix.*`                  | `events.Bus.Publish`                          |
| `event:subscribe`  | Event name or `prefix.*`                  | `events.Bus.Subscribe`                        |

The contract is in [`manifest/manifest.go`](../manifest/manifest.go) (type
`Capability`) and the enforcement in
[`security/context.go`](../security/context.go) (type `Capabilities`).

### Compiled policy

At install time the manifest entries are compiled into a
`security.Capabilities` policy:

```go
caps := security.Compile(addonKey, manifest.Capabilities)
```

Two implicit grants are always added:

- `db:read addon_<key>.*` — every addon may read its own schema.
- `db:write addon_<key>.*` — every addon may write its own schema.

`http:fetch` targets are validated to be **registrable domains**:
bare `*`, `*.com`, leftover wildcards, and other dangerous patterns are
silently dropped. SSRF guards reject loopback, RFC1918 ranges
(`10.*`, `172.16-31.*`, `192.168.*`) and cloud metadata endpoints
(`169.254.169.254`, `metadata.google.internal`) regardless of the
declared target.

### Enforcer modes

`security.Enforcer` ([`security/enforcer.go`](../security/enforcer.go))
wraps the compiled policy and applies it at each privileged call. Mode is
atomic and switchable at runtime:

| Mode          | Behaviour                                                |
| ------------- | -------------------------------------------------------- |
| `ModeShadow`  | Log violation, return `nil`. Default during rollout.     |
| `ModeEnforce` | Log AND return the violation error. Caller maps to 403.  |

Operators flip via the `METACORE_ENFORCE` env var:

```bash
# Shadow (default)
unset METACORE_ENFORCE

# Enforce
export METACORE_ENFORCE=1
```

`security.ModeFromEnv()` returns `ModeEnforce` when the value is
`1`, `true`, `TRUE`, `yes`, or `YES`. Anything else is shadow.

```go
enf := security.NewEnforcer(func(addonKey string) *security.Capabilities {
    return policyByAddon[addonKey]
})
// Optional metric hook
enf.OnViolation = func(addonKey, kind, target, caller string, err error) {
    metrics.CapabilityViolation.WithLabelValues(addonKey, kind).Inc()
}
```

Every violation logs a structured line:

```
metacore.capability.violation mode=enforce addon=tickets kind=http:fetch \
  target=api.stripe.com caller=runtime/wasm/host.go:142 err=addon "tickets" lacks http:fetch "api.stripe.com"
```

### Walkthrough of a check

A `tickets` addon executes `db:write` on `addon_tickets.tickets`:

1. The host calls `enforcer.CheckCapability("tickets", "db:write", "addon_tickets.tickets")`.
2. The enforcer looks up the compiled policy via
   `LookupCapabilities("tickets")`.
3. Dispatch on kind → `caps.CanWriteModel("addon_tickets.tickets")`.
4. `matchAny(c.dbWrite, "addon_tickets.tickets")` — matches the implicit
   `addon_tickets.*` grant → returns `nil`.
5. The kernel proceeds with the DB write.

If the addon had instead tried `db:write addon_other.*`:

1. `matchAny(c.dbWrite, "addon_other.x")` returns false.
2. Enforcer logs the violation.
3. In `ModeShadow`: returns `nil`, the call proceeds (audit-only). Metrics
   tick.
4. In `ModeEnforce`: returns the error, the host import fails, the addon
   sees an "operation denied" return value.

## Best practices

- **Start in shadow.** Ship every new release with `ModeShadow` for one
  rollout window. Inspect violation logs before flipping.
- **Wire `OnViolation` to metrics.** A Prometheus counter labelled by
  `addon` + `kind` shows the real-traffic surface of the cap system —
  invaluable when authoring a new addon.
- **Declare specific targets.** Prefer `addon_tickets.tickets` over
  `addon_tickets.*` when the addon really only writes one table; the
  marketplace surface gets smaller.
- **`http:fetch` needs a registrable domain.** `*.example.com` is fine,
  `*.com` is rejected. The enforcer is paranoid by design.
- **Least-privilege roles.** Grant `<resource>.read` widely and
  `<resource>.delete` narrowly. Use the per-user override store for the
  rare exceptions.
- **Cache invalidation.** Call `permission.Service.InvalidateUser(uid)`
  after any role change for that user; `InvalidateAll()` after a
  role→capability mapping change.
- **Owners are super by default.** If your business needs `admin` to also
  bypass, pass `Config.SuperRoles = []Role{RoleOwner, RoleAdmin}` —
  do **not** grant a `*` capability in the store (super-roles short-circuit
  before the store lookup, which is faster and safer).
- **Use addon caps for transport security.** A `http:fetch` declaration is
  not a UX hint, it is the only thing standing between a malicious bundle
  and your customers' data. Treat marketplace approval as a security gate.

## See also

- [`dynamic-system.md`](dynamic-system.md) — how the user gate fires per CRUD request.
- [`dynamic-api.md`](dynamic-api.md) — `403` response shape.
- [`CONSUMER_GUIDE.md`](CONSUMER_GUIDE.md), section *Capability model and security modes*.
- [`embedding-quickstart.md`](embedding-quickstart.md) — wiring the store from main.go.
- [`../manifest/manifest.go`](../manifest/manifest.go) — manifest type definitions.
- [`../permission/service.go`](../permission/service.go), [`../security/enforcer.go`](../security/enforcer.go) — implementations.
