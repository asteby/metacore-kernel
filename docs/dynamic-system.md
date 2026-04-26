<p align="center">
  <img src="assets/metacore.svg" width="120" alt="Metacore Kernel" />
</p>

<h1 align="center">Dynamic CRUD Framework</h1>

<p align="center">
  <em>From <code>manifest.json</code> to a working CRUD UI in zero lines of glue code.</em>
</p>

---

## Table of contents

- [The promise](#the-promise)
- [Mental model](#mental-model)
- [End-to-end walkthrough](#end-to-end-walkthrough)
  - [1. Declare the model](#1-declare-the-model)
  - [2. Install the addon](#2-install-the-addon)
  - [3. Endpoints exposed automatically](#3-endpoints-exposed-automatically)
  - [4. Metadata for the UI](#4-metadata-for-the-ui)
  - [5. Frontend rendering](#5-frontend-rendering)
- [Schema isolation and RLS](#schema-isolation-and-rls)
- [Permission gates](#permission-gates)
- [Real-time updates](#real-time-updates)
- [What is NOT auto](#what-is-not-auto)
- [See also](#see-also)

---

## The promise

When an addon ships a `model_definitions[]` block in its `manifest.json`, the
kernel does the entire substrate work behind the resulting CRUD page:

| You declare in manifest               | Kernel produces                                                    |
| ------------------------------------- | ------------------------------------------------------------------ |
| `model_definitions[].table_name`      | `CREATE TABLE addon_<key>.<table>` in the addon's isolated schema  |
| `model_definitions[].columns[]`       | Postgres columns with types, defaults, indexes, unique constraints |
| `org_scoped: true`                    | `organization_id` column + Row-Level Security policy               |
| `soft_delete: true`                   | `deleted_at` column                                                |
| (model registered via `RegisterModel`)| `GET/POST/PUT/DELETE /dynamic/<model>` Fiber routes                |
| `model_definitions[].table` UI hint   | `GET /metadata/table/<model>` (TableMetadata)                      |
| `model_definitions[].modal` UI hint   | `GET /metadata/modal/<model>` (ModalMetadata)                      |
| `capabilities[].kind`                 | Per-request gate via `permission.Service`                          |

Frontends consuming `@asteby/metacore-runtime-react` render the table, the
create/edit modal and the option pickers from those metadata responses. No
custom React, no per-model handlers, no glue migrations.

## Mental model

```
manifest.json                          Kernel                              HTTP
─────────────                          ──────                              ────
                                                                         GET  /metadata/table/:model
model_definitions[]  ──┐               ┌─► metadata.Service ─────────►   GET  /metadata/modal/:model
                       │               │                                  GET  /metadata/all
                       ▼               │
                   installer.Install   │                                  GET    /dynamic/:model
                       │               │                                  POST   /dynamic/:model
                       ├──► dynamic.CreateTable (DDL)                     GET    /dynamic/:model/:id
                       ├──► dynamic.SyncSchema  (ALTER ADD COLUMN)        PUT    /dynamic/:model/:id
                       └──► modelbase.Register (model factory)            DELETE /dynamic/:model/:id
                                       │
                                       ├─► dynamic.Service ─────────────► (handler.Mount)
                                       │                                  GET /options/:model
                                       │                                  GET /search/:model
capabilities[]      ──► permission.Service ◄──── per-request gate
                        security.Enforcer  ◄──── addon-side guard
```

The framework is **declarative at the boundary** (manifest, capabilities,
metadata) and **reflective inside** — `dynamic.BuildStructType` synthesises a
GORM-compatible struct from the column list at runtime, so a kernel upgrade
that adds a new column type lights up every existing addon without rebuilding
addon binaries.

## End-to-end walkthrough

We will follow a hypothetical `tickets` addon. Every snippet that follows is
a literal artefact in the install flow — no pseudo-code.

### 1. Declare the model

```json
{
  "key": "tickets",
  "name": "Tickets",
  "version": "0.1.0",
  "kernel": ">=0.2.0",
  "tenant_isolation": "shared",

  "model_definitions": [
    {
      "table_name": "tickets",
      "model_key":  "tickets",
      "label":      "Tickets",
      "org_scoped": true,
      "soft_delete": true,
      "columns": [
        { "name": "subject",  "type": "string",  "size": 200, "required": true, "index": true },
        { "name": "status",   "type": "string",  "size": 24,  "required": true, "default": "'open'" },
        { "name": "priority", "type": "string",  "size": 12,  "default": "'normal'" },
        { "name": "body",     "type": "text" },
        { "name": "due_at",   "type": "timestamp" }
      ],
      "table": {
        "title": "Tickets",
        "searchColumns": ["subject"],
        "columns": [
          { "key": "subject",  "label": "Subject",  "type": "text",   "sortable": true },
          { "key": "status",   "label": "Status",   "type": "badge",  "filterable": true },
          { "key": "priority", "label": "Priority", "type": "badge",  "filterable": true },
          { "key": "due_at",   "label": "Due",      "type": "date",   "sortable": true }
        ],
        "enableCRUDActions": true
      },
      "modal": {
        "title": "Ticket",
        "fields": [
          { "key": "subject",  "label": "Subject",  "type": "text",     "required": true },
          { "key": "status",   "label": "Status",   "type": "select",
            "options": [
              { "value": "open",     "label": "Open" },
              { "value": "pending",  "label": "Pending" },
              { "value": "resolved", "label": "Resolved" }
            ]
          },
          { "key": "priority", "label": "Priority", "type": "select",
            "options": [
              { "value": "low",    "label": "Low" },
              { "value": "normal", "label": "Normal" },
              { "value": "high",   "label": "High" }
            ]
          },
          { "key": "body",   "label": "Body",  "type": "textarea" },
          { "key": "due_at", "label": "Due",   "type": "date" }
        ]
      }
    }
  ],

  "capabilities": [
    { "kind": "db:read",  "target": "addon_tickets.*", "reason": "Read own tickets" },
    { "kind": "db:write", "target": "addon_tickets.*", "reason": "Create and edit tickets" }
  ]
}
```

The complete schema is in [`manifest/manifest.go`](../manifest/manifest.go).
Allowed column types are: `string` (varchar with `size`), `text`, `uuid`,
`int` / `integer`, `bigint`, `decimal` / `numeric` / `float` / `double`,
`bool` / `boolean`, `timestamp` / `datetime`, `date`, `jsonb` / `json`. See
[`dynamic/model.go`](../dynamic/model.go) for the canonical mapping.

### 2. Install the addon

A host (link, ops, ...) calls the installer with a parsed bundle:

```go
inst, secret, err := h.Installer.Install(orgID, bundle)
```

`installer.Install` ([`installer/installer.go`](../installer/installer.go))
runs, in order:

1. `bundle.Manifest.Validate(kernelVersion)` — semver compatibility check.
2. `dynamic.EnsureSchema` — `CREATE SCHEMA IF NOT EXISTS addon_tickets`.
3. `dynamic.Apply` — runs every versioned SQL migration shipped with the
   bundle, in a transaction, with checksum locking.
4. For each `ModelDefinition`:
   - `dynamic.CreateTable` — `CREATE TABLE IF NOT EXISTS addon_tickets.tickets
     (...)`. When the manifest declares `tenant_isolation: shared` and the
     definition is `org_scoped`, the kernel also enables Postgres Row-Level
     Security with a policy keyed on `current_setting('app.current_org')`.
   - `dynamic.SyncSchema` — `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` for any
     column the manifest declares but the existing table is missing
     (additive-only; renames and drops require explicit migrations).
5. Lifecycle hooks: `OnInstall` then `OnEnable`.
6. Persists a `metacore_installations` row with a fresh per-installation
   HMAC secret (returned to the caller, hashed in DB).

There is **no `metacore migrate` CLI**. Migration runs as a side effect of
installing the addon, and is fully idempotent — re-running the same install
is a no-op.

### 3. Endpoints exposed automatically

Once the host calls `app.RegisterModel("tickets", factory)` and mounts the
`host.App` ([`host/app.go`](../host/app.go)), every request below is wired
without further code:

| Method | Path                          | Behaviour                                                       |
| ------ | ----------------------------- | --------------------------------------------------------------- |
| GET    | `/api/dynamic/tickets`        | Paginated list (`?page`, `?per_page`, `?sortBy`, `?order`, `?search`, `?f_<col>`) |
| POST   | `/api/dynamic/tickets`        | Create. Body is a JSON object keyed by column name              |
| GET    | `/api/dynamic/tickets/:id`    | Get one. 404 when missing or filtered out by org scope          |
| PUT    | `/api/dynamic/tickets/:id`    | Update. Partial JSON body — only present keys are written       |
| DELETE | `/api/dynamic/tickets/:id`    | Soft delete when `soft_delete: true`, hard delete otherwise     |
| GET    | `/api/options/tickets`        | Render `<select>` options (`?field=<col>`) — needs `OptionsConfigResolver` and an explicit `MountOptions` call |
| GET    | `/api/search/tickets`         | Full-text search (`?q=`) — needs `SearchConfigResolver` and an explicit `MountOptions` call |

Every successful response is wrapped:

```json
{ "success": true, "data": ..., "meta": { /* list only */ } }
```

Errors:

```json
{ "success": false, "message": "permission denied" }
```

The full request/response reference, including curl examples, is in
[`dynamic-api.md`](dynamic-api.md).

### 4. Metadata for the UI

The metadata endpoints feed the table view and the modal form. They never
hit the addon's data tables — they describe the **shape** of the UI.

```bash
curl -H "Authorization: Bearer $JWT" \
  https://api.example.com/api/metadata/table/tickets
```

```json
{
  "success": true,
  "data": {
    "title": "Tickets",
    "columns": [
      { "key": "subject",  "label": "Subject",  "type": "text",  "sortable": true },
      { "key": "status",   "label": "Status",   "type": "badge", "filterable": true },
      { "key": "priority", "label": "Priority", "type": "badge", "filterable": true },
      { "key": "due_at",   "label": "Due",      "type": "date",  "sortable": true }
    ],
    "searchColumns": ["subject"],
    "enableCRUDActions": true
  }
}
```

```bash
curl -H "Authorization: Bearer $JWT" \
  https://api.example.com/api/metadata/modal/tickets
```

```json
{
  "success": true,
  "data": {
    "title": "Ticket",
    "fields": [
      { "key": "subject",  "label": "Subject",  "type": "text",     "required": true },
      { "key": "status",   "label": "Status",   "type": "select",
        "options": [
          { "value": "open",     "label": "Open" },
          { "value": "pending",  "label": "Pending" },
          { "value": "resolved", "label": "Resolved" }
        ]
      },
      { "key": "priority", "label": "Priority", "type": "select",   "options": [/* … */] },
      { "key": "body",     "label": "Body",     "type": "textarea" },
      { "key": "due_at",   "label": "Due",      "type": "date" }
    ]
  }
}
```

The exact Go shapes of `TableMetadata`, `ModalMetadata`, `ColumnDef`, and
`FieldDef` live in [`modelbase/metadata.go`](../modelbase/metadata.go) — they
are part of the kernel's public API and any change to a JSON tag is a MAJOR
version bump.

The metadata service caches both responses for `MetadataCacheTTL`
(default 5 min). Hosts call `metaSvc.InvalidateModel("tickets")` after an
admin edits the per-org overlay.

### 5. Frontend rendering

A host running `@asteby/metacore-runtime-react` mounts a generic page:

```tsx
import { DynamicTable } from "@asteby/metacore-runtime-react";

export default function TicketsPage() {
  return <DynamicTable model="tickets" />;
}
```

The component:

1. Fetches `GET /metadata/table/tickets` to learn the columns, filters and
   sort policy.
2. Fetches `GET /metadata/modal/tickets` once when the user opens "New".
3. Issues `GET /dynamic/tickets?page=1&per_page=25&sortBy=due_at&order=asc`
   on mount and on every filter/sort change.
4. On submit of the create form, `POST /dynamic/tickets`. On row edit,
   `PUT /dynamic/tickets/:id`. On delete, `DELETE /dynamic/tickets/:id`.

End to end: **zero per-model frontend code** for the 80 % case. See the SDK
guide at `metacore-sdk/docs/CONSUMER_GUIDE.md` for advanced rendering
(custom cells, row actions, inline editing).

## Schema isolation and RLS

Every addon owns a private Postgres schema. The naming follows
[`dynamic/isolation.go`](../dynamic/isolation.go):

| Manifest `tenant_isolation` | Schema layout                              | Cross-org access |
| --------------------------- | ------------------------------------------ | ---------------- |
| `shared` (default)          | `addon_<key>`                              | RLS-policed      |
| `schema-per-tenant`         | `addon_<key>_<8 hex chars of orgID>`       | Impossible       |
| `database-per-tenant`       | reserved                                   | n/a              |

**Shared isolation** is the default and the right choice for most addons.
The kernel:

- adds `organization_id uuid NOT NULL` to every `org_scoped` table;
- creates an index on `organization_id`;
- enables `ROW LEVEL SECURITY` on the table;
- installs a policy that filters every `SELECT/UPDATE/DELETE` on
  `organization_id = current_setting('app.current_org')::uuid`.

Hosts MUST call `dynamic.SetRequestOrg(db, orgID)` (or the equivalent
`SET LOCAL app.current_org = '<uuid>'`) inside every request transaction
that touches a shared addon table — otherwise the policy filters everything
out and the request returns an empty list. The recommended pattern is a
Fiber middleware that wraps every request in a transaction with the GUC set.

**Schema-per-tenant** trades the runtime guard for a hard boundary:
disjoint schemas mean no cross-org leak is even representable in SQL. Use it
for regulated data (clinical, fiscal) where the audit story is "two
organisations cannot share a row by construction".

## Permission gates

The kernel ships **two cooperating permission systems**:

1. **`permission.Service`** ([`permission/service.go`](../permission/service.go))
   — gates every dynamic CRUD request on a per-user, per-action capability.
   Triggered automatically by `dynamic.Service` when the host wires a
   `PermissionStore`.
2. **`security.Enforcer`** ([`security/enforcer.go`](../security/enforcer.go))
   — gates every privileged action an *addon* attempts (db read, http fetch,
   event emit). Independent of the user-level system.

### User-level gate (the per-request CRUD guard)

When the host wires `host.AppConfig.PermissionStore`, every CRUD request
runs `permission.Service.Check(ctx, user, Cap(model, action))` before
talking to the database. The capability shape is `<resource>.<action>`:

| HTTP                                | Capability checked  |
| ----------------------------------- | ------------------- |
| `GET    /api/dynamic/tickets`       | `tickets.read`      |
| `GET    /api/dynamic/tickets/:id`   | `tickets.read`      |
| `POST   /api/dynamic/tickets`       | `tickets.create`    |
| `PUT    /api/dynamic/tickets/:id`   | `tickets.update`    |
| `DELETE /api/dynamic/tickets/:id`   | `tickets.delete`    |

A failing check returns `403 Forbidden`:

```json
{
  "success": false,
  "message": "permission denied: missing capability \"tickets.create\""
}
```

`RoleOwner` is in `DefaultSuperRoles()` and bypasses every check (a single
synthetic `*` capability is returned for the user). Hosts that want
`admin` to also bypass set `Config.SuperRoles` explicitly.

Capability grants are owned by a `PermissionStore`:

- `permission.InMemoryStore` — for tests and apps that seed roles at boot.
- `permission.GormStore` — production default; persists in
  `permission_role_grants` and `permission_user_grants`. Includes
  `GrantRole`, `GrantUser`, `RevokeRole`, `RevokeUser` helpers.

Mounting a Fiber gate manually (for non-CRUD routes):

```go
api.Post("/tickets/:id/escalate",
    permSvc.Gate(userLookup, permission.Cap("tickets", "escalate")),
    ticketHandler.Escalate)
```

`Gate` (single cap) and `GateWith` (multi-cap with `ModeAll`/`ModeAny`) are
in [`permission/middleware.go`](../permission/middleware.go).

### Addon-level gate (capability enforcement)

`security.Enforcer` validates that an addon stays within the capabilities it
declared in its manifest. The enforcer is consulted from inside the kernel's
host imports (DB read, HTTP fetch, event publish) before the privileged op
runs:

```go
if err := enforcer.CheckCapability("tickets", "db:write", "addon_tickets.tickets"); err != nil {
    return err
}
```

Mode is global, atomic, and switchable at runtime:

| Mode          | Behaviour                                        |
| ------------- | ------------------------------------------------ |
| `ModeShadow`  | Log violations, never block. Default.            |
| `ModeEnforce` | Log AND return an error. Caller maps to 403.     |

Operators flip via `METACORE_ENFORCE=1` (see
[`security/enforcer.go`](../security/enforcer.go) `ModeFromEnv`). Every
violation also calls `Enforcer.OnViolation` if set — wire it to a Prometheus
counter for an audit feed.

The full reference is in [`permissions.md`](permissions.md).

## Real-time updates

The dynamic CRUD layer does **not** automatically broadcast changes to
WebSocket clients. The kernel ships a hub
([`ws/hub.go`](../ws/hub.go)) and the host's CRUD handler is free to call it,
but the contract is: **mutation handlers fan out themselves**.

Recommended pattern — wrap the dynamic service from the host:

```go
// In the host app: wrap Create/Update/Delete to broadcast.
type ticketsRealtime struct {
    dyn *dynamic.Service
    hub *ws.Hub
}

func (r *ticketsRealtime) Create(ctx context.Context, user modelbase.AuthUser, in map[string]any) (map[string]any, error) {
    out, err := r.dyn.Create(ctx, "tickets", user, in)
    if err != nil {
        return nil, err
    }
    // Look up the recipients however your domain dictates.
    recipients := orgUserIDs(ctx, user.GetOrganizationID())
    r.hub.SendToUsers(recipients, ws.Message{
        Type:    "TICKET_CREATED",
        Payload: out,
    })
    return out, nil
}
```

`Hub.SendToUsers` is fire-and-forget and non-blocking. Persisting
notifications is delegated to `Hub.OnNotification`; wire it if your app needs
durable storage.

For cross-process fan-out (multi-replica deployment), use the addon
event bus ([`events/`](../events/)) and have each replica subscribe to its
own forwarder — the in-process hub is per-process by design.

## What is NOT auto

The dynamic framework draws a deliberate line. The following are explicitly
**not** generated for you and have to be implemented by addon code or by the
host:

| Concern                                     | Where to put it                                                  |
| ------------------------------------------- | ---------------------------------------------------------------- |
| Custom validation (cross-field, async)      | `dynamic.Hooks.BeforeCreate` / `BeforeUpdate` — see [`dynamic/hooks.go`](../dynamic/hooks.go) |
| Joins, computed columns, denormalisation    | Either a SQL view exposed as a separate model, or a custom Fiber handler |
| Custom row actions ("escalate", "mark paid")| Addon-defined endpoint + `manifest.Actions[]` for the UI button  |
| Authorization beyond `<resource>.<action>`  | Wrap the service or implement a custom `PermissionStore`         |
| Cross-replica WebSocket broadcast           | Host responsibility — fan out via `Hub.SendToUsers` per replica  |
| Field-level encryption / redaction          | `metadata.TableTransformer` to hide; addon hook to encrypt       |
| Schema migrations beyond ADD COLUMN         | Versioned SQL files in the bundle (`dynamic.Apply` runs them)    |
| File uploads / blob storage                 | Out of scope for the dynamic layer — handle in addon endpoints   |

Everything that **is** auto fits in one principle: it can be derived from
the manifest without running addon code. Anything that needs a decision the
manifest cannot encode goes in addon code, where you keep full control.

## See also

- [`dynamic-api.md`](dynamic-api.md) — full HTTP API reference with curl examples.
- [`permissions.md`](permissions.md) — capability model, modes, store implementations.
- [`embedding-quickstart.md`](embedding-quickstart.md) — your first host with the kernel embedded.
- [`CONSUMER_GUIDE.md`](CONSUMER_GUIDE.md) — long-form embedding guide.
- [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — the four laws of the kernel.
