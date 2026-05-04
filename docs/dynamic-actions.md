# Dynamic Actions API (proposal — kernel-side)

HTTP reference for the per-row action endpoint that the dynamic CRUD
framework will mount alongside list/get/create/update/delete:

```
POST /dynamic/:model/:id/action/:key
```

This document is the wire contract and dispatch contract for that endpoint.
It is the v1 spec — **proposal, no implementation yet** — and depends on:

- [`docs/audits/2026-05-04-action-trigger-gap.md`](audits/2026-05-04-action-trigger-gap.md)
  for the `manifest.ActionDef.Trigger` shape this endpoint dispatches on.
- [`docs/wasm-abi.md`](wasm-abi.md) §10 (`db_exec`), §12 (`event_emit`) for the
  guest-facing rules when the trigger is `wasm` with `RunInTx=true`.
- [`docs/dynamic-api.md`](dynamic-api.md) for the surrounding CRUD endpoints
  whose conventions this one inherits (auth, error envelope, status codes,
  org scoping).

For the CRUD walkthrough see [`dynamic-system.md`](dynamic-system.md).

---

## Table of contents

- [Overview](#overview)
- [Route](#route)
- [Authentication](#authentication)
- [URL parameters](#url-parameters)
- [Request body](#request-body)
- [Resolution order](#resolution-order)
- [Dispatch flow](#dispatch-flow)
- [Trigger types](#trigger-types)
  - [Type=wasm](#typewasm)
  - [Type=webhook](#typewebhook)
  - [Type=event](#typeevent)
  - [Type=none](#typenone)
- [Phases](#phases)
- [Transaction lifecycle (RunInTx)](#transaction-lifecycle-runintx)
- [Response envelope](#response-envelope)
- [Error codes](#error-codes)
- [Status code reference](#status-code-reference)
- [Idempotency](#idempotency)
- [Audit](#audit)
- [Capabilities](#capabilities)
- [Limits](#limits)
- [Out of scope for v1](#out-of-scope-for-v1)
- [Tests (proposed coverage)](#tests-proposed-coverage)

---

## Overview

A **dynamic action** is a server-side reaction declared by an addon next to
its model. The UI renders a button (or modal, or context-menu entry) for
each `ActionDef` returned by the table metadata; clicking it `POST`s to
this endpoint. The kernel:

1. Loads the target row (org-scoped).
2. Resolves the `ActionDef` and its `Trigger` from the addon manifest.
3. Optionally opens a single DB transaction.
4. Dispatches to the trigger backend (today: WASM in-process, signed
   webhook, internal event bus, or no-op for UI-only actions).
5. Commits or rolls back, then writes a kernel envelope back to the caller.

The endpoint is the **canonical entry point** for any per-row reaction in
the system. Direct webhook fan-out (`bridge/actions.go:signedActionInterceptor`)
becomes one of several `Trigger.Type` branches behind it; existing addons
keep working unchanged because `Trigger == nil` falls through to the legacy
webhook path (see [Trigger types §webhook](#typewebhook)).

## Route

```
POST /dynamic/:model/:id/action/:key
```

Mounted by `dynamic.Handler` ([`dynamic/handler.go`](../dynamic/handler.go))
inside the `/dynamic` group, after the existing `:model/:id` routes. Full
path under the conventional `/api` mount: `POST /api/dynamic/tickets/<uuid>/action/escalate`.

The route lives on the dynamic handler — not on a separate `actions/`
group — because it is logically a write to a known row of a known model:
it shares auth, model registry resolution, org scoping, and error mapping
with `PUT /:model/:id`.

## Authentication

Same as every other `/dynamic` endpoint (see [`dynamic-api.md`](dynamic-api.md#authentication)).
The JWT is required:

```
Authorization: Bearer <jwt>
```

Missing/invalid → `401 {"success":false,"message":"not authenticated"}`.

The user resolved by `dynamic.UserResolver` is threaded as `principal`
into the trigger invocation (see [§Type=wasm](#typewasm) and
[§Type=webhook](#typewebhook)).

## URL parameters

| Param   | Type   | Notes                                                                       |
| ------- | ------ | --------------------------------------------------------------------------- |
| `model` | string | Registry key passed to `App.RegisterModel(key, factory)` — same as CRUD.    |
| `id`    | uuid   | The row id. Non-UUID returns `400 invalid id`.                              |
| `key`   | string | The `ActionDef.Key` from the model's manifest. Unknown returns `404`.       |

`:key` matches against `ActionDef.Key`, **not** `ActionDef.Name`. `Key` is
the stable identifier; `Name` is a humanized label for telemetry and i18n.

## Request body

JSON object. Two top-level shapes are supported; the kernel accepts either
and normalises internally:

**Compact shape** — body is the raw `payload`:

```json
{
  "reason": "Customer escalation requested",
  "notify": true
}
```

**Explicit shape** — body wraps `payload` + per-call options:

```json
{
  "payload": {
    "reason": "Customer escalation requested",
    "notify": true
  },
  "client_request_id": "8b1f3de0-…"
}
```

The compact shape is the default; consumers that need to send a
client-supplied idempotency key (or, in v1.2, a `dry_run: true` flag) use
the explicit shape. Bodies that include any reserved key (`payload`,
`client_request_id`, `dry_run`) at the top level are interpreted as the
explicit shape; everything else as compact.

Validation:

- The body MUST validate against `ActionDef.Fields` (same
  `modelbase.FieldDef[]` shape used by the modal metadata endpoint). The
  kernel coerces JSON onto the runtime struct produced by
  `dynamic.BuildStructType` exactly the same way `Create` / `Update` do.
- Type mismatches surface as `400 invalid_input` — same path as `Create`.
- Cross-field validation belongs to the trigger (the WASM guest, the
  webhook handler, …); the kernel only enforces shape.

When `ActionDef.Fields` is empty the body is forwarded verbatim — the
trigger gets whatever the client sent.

## Resolution order

Before any side-effect runs, the kernel resolves the action in this order
(failures short-circuit; the first hit wins the corresponding error code):

1. **Model registered.** `:model` ∈ `App.Registry`. Else
   `404 model_not_found`.
2. **ID well-formed.** `uuid.Parse(:id)`. Else `400 invalid_id`.
3. **Row exists & visible.** Org-scoped load via `service.Get(ctx, model,
   user, id)`. Else `404 record_not_found`. The same scope rules CRUD
   uses apply (soft-deleted rows are invisible).
4. **Action declared.** `:key` ∈ `manifest.Models[<model>].Actions[*].Key`.
   Else `404 action_not_found`.
5. **State precondition.** If `ActionDef.RequiresState` is non-empty, the
   row's `status` (or whatever column is configured as the state field —
   see `ActionDef.RequiresState`) must be one of the listed values. Else
   `409 invalid_state`.
6. **Capability.** If `Trigger.RequiredCapability` is set, the principal
   must hold it. Else `403 forbidden`. (See [§Capabilities](#capabilities).)
7. **Body shape.** Validate against `ActionDef.Fields`. Else
   `400 invalid_input`.
8. **Trigger type & sub-rules.** See [§Trigger types](#trigger-types).

Steps 1–4 are pure reads with no side effect; step 5 reads only the loaded
row. Steps 6–8 happen inside (or just before) the transaction window per
[§Dispatch flow](#dispatch-flow).

## Dispatch flow

The handler runs the four-step contract from the task statement:

```
   ┌── 1. load row ────────┐                ┌── 4a. commit if guest ok ──┐
   │  service.Get(...)     │                │   tx.Commit()              │
   │  org-scoped, locked   │                └────────────────────────────┘
   └─────────┬─────────────┘                ┌── 4b. rollback otherwise ──┐
             │                              │   tx.Rollback()            │
   ┌─────────▼─────────────┐                └────────────────────────────┘
   │  2. open tx           │
   │  db.Transaction(...)  │
   │  + SET LOCAL search   │
   │    path / timeout     │
   └─────────┬─────────────┘
             │
   ┌─────────▼──────────────────────────────────────────────┐
   │  3. dispatch on Trigger.Type                           │
   │     wasm → Host.InvokeInTx(ctx, tx, install, fn,       │
   │             {row, args, ctx}, settings, principal)     │
   │     webhook → signed POST (no tx semantics, see below) │
   │     event   → bus.Publish (no tx semantics, see below) │
   │     none    → no-op                                    │
   └────────────────────────────────────────────────────────┘
```

Pseudocode of the handler skeleton (`dynamic/handler.go`, new
`actionDispatch` method — non-normative):

```go
func (h *Handler) action(c fiber.Ctx) error {
    u := h.user(c)
    if u == nil { return respondErr(c, 401, "not authenticated") }

    id, err := uuid.Parse(c.Params("id"))
    if err != nil { return respondErr(c, 400, ErrInvalidID.Error()) }

    row, def, trig, err := h.service.LoadActionContext(c,
        c.Params("model"), u, id, c.Params("key"))
    if err != nil { return h.handleError(c, err) }

    var input map[string]any
    if err := c.Bind().Body(&input); err != nil {
        return respondErr(c, 400, "invalid body")
    }
    payload, clientReqID := splitBody(input) // see "Request body"
    if err := validateAgainstFields(payload, def.Fields); err != nil {
        return respondErr(c, 400, err.Error())
    }
    if !satisfiesState(row, def.RequiresState) {
        return respondErr(c, 409, "invalid_state")
    }
    if !u.HasCapability(trig.RequiredCapability) {
        return respondErr(c, 403, "forbidden")
    }

    return h.runActionTrigger(c, actionRequest{
        Row:           row,
        Def:           def,
        Trigger:       trig,
        Payload:       payload,
        Principal:     buildPrincipal(c, u),
        ClientReqID:   clientReqID,
        ModelKey:      c.Params("model"),
        ActionKey:     def.Key,
    })
}
```

`runActionTrigger` is where the four-step contract lives — it owns the
optional `db.Transaction` envelope, the per-`Trigger.Type` branch, the
envelope construction, and the audit/idempotency hooks.

## Trigger types

### Type=wasm

Default for actions whose addon ships a WASM backend
(`manifest.Backend.Runtime == "wasm"`). The kernel:

1. Opens `db.Transaction(...)` — when `Trigger.RunInTx == true`. When
   `false`, the kernel uses the regular `*gorm.DB` handle (no transaction
   reuse; the guest's `db_exec` calls fail with `no_active_tx` per
   [`wasm-abi.md` §10.6](wasm-abi.md#106-transaction-reuse--the-gormdb-contract)).
2. Inside the tx, runs `SET LOCAL search_path TO addon_<key>, public` and
   `SET LOCAL statement_timeout = '<n>s'` — `n` = `min(Trigger.TimeoutMs,
   BackendSpec.TimeoutMs)` rounded down to the nearest second, default
   `5s`. Same convention as `db_exec` already mandates.
3. Builds the **WASM action envelope** described below and serialises it
   to JSON.
4. Calls `Host.InvokeInTx(ctx, tx, installation, addonKey, fn, payload,
   settings, principal)` for `RunInTx=true`, or `Host.Invoke(ctx,
   installation, addonKey, fn, payload, settings, principal)` otherwise.
5. Reads back the guest's packed `(ptr, len)` envelope, JSON-decodes it,
   and applies the [auto-rollback contract](#transaction-lifecycle-runintx).

#### Wasm action envelope (request to guest)

```json
{
  "row": {
    "id": "9b1c08f1-…",
    "organization_id": "11111111-…",
    "subject": "Invoice #2042 missing PDF",
    "status": "open",
    "priority": "high",
    "created_at": "2026-04-26T12:01:09Z",
    "updated_at": "2026-04-26T12:01:09Z"
  },
  "args": {
    "reason": "Customer escalation requested",
    "notify": true
  },
  "ctx": {
    "model":            "tickets",
    "action":           "escalate",
    "installation_id":  "5f2a1c40-…",
    "principal": {
      "user_id":  "55a8e4c8-…",
      "org_id":   "11111111-…",
      "locale":   "es-MX"
    },
    "request_id":       "req_01HXY…",
    "idempotency_key":  "8b1f3de0-…"
  }
}
```

- `row` is the **loaded snapshot** as of step 1 of the dispatch flow,
  serialised the same way `GET /:model/:id` would. The guest reads it
  read-only — mutations land via `db_exec` against the addon schema, not
  by mutating `row` in memory.
- `args` is the validated `payload` from the request body. Empty object
  when the action has no `Fields`.
- `ctx.installation_id` lets the guest call `env_get` consistently if it
  needs settings unique to the installation.
- `ctx.request_id` is the kernel's request id (`X-Request-Id` header
  passthrough or kernel-minted UUID); used for log correlation.
- `ctx.idempotency_key` is the resolved key — see [§Idempotency](#idempotency).

The envelope is a stable JSON shape: addons can deserialise it into a
strongly-typed struct and the kernel commits to additive evolution
(MAJOR bump on field removal/renaming).

#### Wasm response envelope (guest → kernel)

The guest follows the kernel `{success, data, meta}` convention:

```json
{ "success": true,  "data": { …addon-defined… }, "meta": { …addon-defined… } }
{ "success": false, "error": { "code": "…", "message": "…" } }
```

The kernel:

- On `success: true` → commits `tx` (when `RunInTx`), forwards `data` /
  `meta` verbatim into the kernel envelope. `meta` from the guest is
  merged with kernel-managed meta (`rolled_back`, `tx_id`, `audit_id`,
  `latency_ms`) — kernel-managed keys win on collision.
- On `success: false` → rolls back `tx` (when `RunInTx`), forwards
  `error` to the caller, returns HTTP `422` (action declined by the
  guest with a structured error) or the mapped status from
  [§Error codes](#error-codes).

#### Trigger validation (manifest cross-checks)

For `Trigger.Type == "wasm"` the validator (today
`manifest/validate.go:validateBackend` — to be extended per audit
[`2026-05-04-action-trigger-gap.md`](audits/2026-05-04-action-trigger-gap.md)
gap #12) MUST enforce:

- `manifest.Backend.Runtime == "wasm"`.
- `Trigger.Function != ""` and `Trigger.Function ∈ Backend.Exports`.
- `Trigger.RunInTx == true → Trigger.Async == false`. (Async + tx is
  meaningless because the response is decoupled from commit.)
- `Trigger.RunInTx == true → Trigger.Phase ∈ {"before", "instead_of"}`.
  `"after"` + `RunInTx` is rejected because the kernel's CRUD mutation
  has already committed by then.
- `Trigger.TimeoutMs ≤ Backend.TimeoutMs` (lower wins; the action cannot
  request more wall time than the backend module is permitted).

### Type=webhook

Backwards-compatible legacy path. Used when:

- The manifest does not declare `Trigger` at all (default), **and**
  `manifest.Hooks["<model>::<action>"]` resolves to a URL — same rule
  `bridge/actions.go:75-83` already applies.
- Or the manifest sets `Trigger.Type == "webhook"` explicitly (with
  optional `Trigger.URL` overriding `manifest.Hooks`).

The kernel does NOT open a DB transaction — the webhook handler runs in a
remote process and cannot participate in a local tx. `Trigger.RunInTx`
combined with `Type == "webhook"` is a validator error.

The body POSTed to the webhook is the same envelope shape used today by
`bridge/actions.go:marshalActionBody`:

```json
{
  "record_id": "9b1c08f1-…",
  "payload":   { "reason": "…", "notify": true },
  "hook":      "tickets::escalate",
  "org_id":    "11111111-…"
}
```

Plus the v1 additions:

```json
{
  "principal": { "user_id": "…", "org_id": "…", "locale": "es-MX" },
  "ctx":       { "request_id": "req_01HXY…", "idempotency_key": "…" },
  "model":     "tickets",
  "action":    "escalate"
}
```

Signed with the existing HMAC scheme. The remote response is forwarded
into the kernel envelope's `data` if it parses as `{success, data, meta}`,
otherwise wrapped as `{success: true, data: <body>}` for backwards
compatibility with addons that return raw JSON.

### Type=event

Fire-and-forget. The kernel:

1. Loads the row (step 1 of dispatch flow).
2. Optionally validates `Trigger.Event` is declared in `manifest.Events`.
3. Calls `events.Bus.Publish(ctx, addonKey, Trigger.Event, orgID,
   payload)` where `payload` is the same WASM action envelope from
   [§Type=wasm](#typewasm) (uniform shape so any subscriber — wasm
   handler, kernel-trusted code, observability — can decode without
   special-casing).
4. Returns `{success: true, data: {event, subscribers}, meta: {...}}`
   immediately. There is no transaction; subscribers run synchronously
   inside `Publish` per the bus contract, but their failures are
   swallowed by the bus and never surface to the caller.

`Trigger.RunInTx` combined with `Type == "event"` is a validator error in
v1 — the bus has no outbox semantics yet (see
[`wasm-abi.md` §12.8](wasm-abi.md#128-side-effect-ordering)). A future
minor version may add publish-on-commit for actions inside `Type == "wasm"`
guests; the standalone `event` trigger stays fire-and-forget.

### Type=none

UI-only. The kernel still loads the row (step 1) for the response payload
but does not dispatch anything. Returns:

```json
{ "success": true, "data": { "row": { … } }, "meta": { "no_op": true } }
```

Used by actions whose entire effect is a frontend modal/link — the round
trip is just a no-op confirmation.

## Phases

`Trigger.Phase` selects when the trigger runs relative to any kernel-side
mutation the action implies. v1 only enforces the values; the kernel's
default behaviour is `"after"` (no kernel-owned mutation):

| Phase         | Kernel behaviour                                                                                       |
| ------------- | ------------------------------------------------------------------------------------------------------ |
| `"before"`    | Open tx → run trigger → if guest succeeded, kernel persists any kernel-owned write (e.g. `last_action_at` bump) inside the same tx → commit. Guest can mutate `row` via `db_exec`. |
| `"after"`     | Default. Open tx → kernel persists its own write (if any) → run trigger → commit. Guest sees the post-mutation row. |
| `"instead_of"`| Open tx → run trigger → commit. The kernel performs no mutation of its own; the guest is responsible for any row write. |

For v1 the kernel does not perform any implicit mutation per action — only
the guest writes — so `before` and `instead_of` are functionally identical.
The distinction is preserved in the manifest because the validator still
enforces `RunInTx + Phase` rules, and v1.x adds (e.g.) "stamp `last_action_at`"
without breaking the wire contract.

## Transaction lifecycle (RunInTx)

When `Trigger.Type == "wasm"` and `Trigger.RunInTx == true`, the kernel
follows the `db_exec` auto-rollback contract verbatim
([`wasm-abi.md` §10.7](wasm-abi.md#107-auto-rollback-contract)):

| Trigger                                                                | tx outcome | HTTP envelope                                                          |
| ---------------------------------------------------------------------- | ---------- | ---------------------------------------------------------------------- |
| Guest returns `{success:true, …}`                                       | Commit     | `200 {success:true, data, meta:{…, rolled_back:false, tx_id}}`         |
| Guest returns `{success:false, error}`                                  | Rollback   | `422 {success:false, error, meta:{rolled_back:true, tx_id}}`           |
| Guest panics / abort-traps                                              | Rollback   | `500 {success:false, error:{code:"runtime_error"}, meta:{rolled_back:true}}` |
| Guest exceeds `timeout_ms`                                              | Rollback   | `504 {success:false, error:{code:"timeout"}, meta:{rolled_back:true}}` |
| Guest exceeds `memory_limit_mb`                                         | Rollback   | `500 {success:false, error:{code:"memory_exhausted"}, meta:{rolled_back:true}}` |
| `db_exec` returns `serialization_failure` and guest re-surfaces it      | Rollback   | `409 {success:false, error:{code:"serialization_failure"}, meta:{retryable:true, rolled_back:true}}` |
| Host context cancelled (request aborted upstream)                      | Rollback   | `499 {success:false, error:{code:"canceled"}}` (or whatever the framework uses for client-aborted) |

`tx_id` is the kernel-assigned id of the action transaction. It is also
threaded into every `db_exec` envelope's `meta.txId` so the guest can
correlate its writes with the host's commit/rollback decision.

The kernel commits **iff** the guest's top-level packed envelope is
`success:true`. Rollback is always safe to repeat; the kernel treats `sql:
transaction has already been committed or rolled back` as a no-op.

When `Trigger.RunInTx == false` for a `wasm` trigger:

- The kernel does not open a tx; the guest runs through `Host.Invoke`.
- The guest's `db_exec` calls fail with `no_active_tx`. Read-only `db_query`
  works.
- `meta.rolled_back` is omitted from the envelope (no tx existed).

For non-wasm triggers (`webhook`, `event`, `none`), `RunInTx` is rejected
by the validator (see [§Type=wasm](#typewasm) cross-checks).

## Response envelope

Success follows the kernel `{success, data, meta}` convention:

```json
{
  "success": true,
  "data": {
    "row": {
      "id": "9b1c08f1-…",
      "subject": "Invoice #2042 missing PDF",
      "status": "escalated",
      "priority": "urgent",
      "updated_at": "2026-04-26T13:42:55Z"
    },
    "result": {
      "ticket_id": "9b1c08f1-…",
      "queue":     "tier-2"
    }
  },
  "meta": {
    "model":         "tickets",
    "action":        "escalate",
    "trigger_type":  "wasm",
    "tx_id":         "tx_01HXY…",
    "rolled_back":   false,
    "idempotent":    false,
    "audit_id":      "aud_01HXZ…",
    "latency_ms":    47
  }
}
```

- `data.row` is included **iff** `Trigger.ReturnsRow == true` or
  `Trigger.Phase != "after"`. The row is re-loaded from the same tx
  immediately before commit so the response reflects the guest's
  mutations without a second round trip and without dirty reads. When
  `RunInTx == false` it is the snapshot from step 1 of the dispatch
  flow.
- `data.result` is whatever the trigger returned in its `data` field —
  forwarded verbatim. Empty object when the trigger returned no data.
- `meta` keys managed by the kernel (`model`, `action`, `trigger_type`,
  `tx_id`, `rolled_back`, `idempotent`, `audit_id`, `latency_ms`) always
  win over keys the trigger placed in its own `meta`. Trigger-supplied
  meta keys are merged otherwise.
- `meta.idempotent: true` when the response was replayed from the
  idempotency cache — see [§Idempotency](#idempotency).

Errors:

```json
{
  "success": false,
  "error": {
    "code":    "invalid_state",
    "message": "ticket is already resolved"
  },
  "meta": {
    "model":         "tickets",
    "action":        "escalate",
    "trigger_type":  "wasm",
    "rolled_back":   true,
    "tx_id":         "tx_01HXY…",
    "latency_ms":    3
  }
}
```

The error envelope's `error.code` is either a kernel code from
[§Error codes](#error-codes) or the verbatim code the trigger returned
(`invalid_state` above is a kernel code; `INSUFFICIENT_BALANCE` from a
guest would be forwarded as-is). `Trigger.ErrorMap` (see audit gap #18)
is applied client-side, not by the kernel.

The error envelope is **not** the same shape used by the rest of
`/dynamic`: existing CRUD endpoints return `{success:false, message}` flat.
The new endpoint returns `{success:false, error:{code, message}, meta}` to
match the `db_exec` / `event_emit` shape — the structured `code` is
load-bearing for the bridge's retry policy and for client-side i18n. Both
handlers continue to coexist; the action endpoint's stricter shape applies
only here.

## Error codes

Kernel-emitted codes (in addition to whatever the trigger forwards):

| Code                    | When                                                                                  |
| ----------------------- | ------------------------------------------------------------------------------------- |
| `model_not_found`       | `:model` not in `App.Registry`.                                                       |
| `action_not_found`      | `:key` not declared on the model's `Actions` list.                                    |
| `record_not_found`      | Row absent or filtered out by tenant scope.                                           |
| `invalid_id`            | `:id` is not a UUID.                                                                  |
| `invalid_input`         | Body fails validation against `ActionDef.Fields`.                                     |
| `invalid_state`         | Row's state column not in `ActionDef.RequiresState`.                                  |
| `forbidden`             | Capability check failed (see [§Capabilities](#capabilities)).                         |
| `runtime_error`         | Guest panicked / abort-trapped.                                                       |
| `timeout`               | Guest exceeded `timeout_ms`.                                                          |
| `memory_exhausted`      | Guest exceeded `memory_limit_mb`.                                                     |
| `serialization_failure` | Underlying SQLSTATE 40001 / 40P01; `meta.retryable: true`.                            |
| `canceled`              | Upstream context cancelled before the guest returned.                                 |
| `bus_unavailable`       | `Type=event` configured but kernel constructed without `*events.Bus`.                 |
| `webhook_unreachable`   | `Type=webhook` POST returned a transport-level failure (no HTTP response).            |
| `webhook_error`         | `Type=webhook` POST returned non-2xx; the body (if JSON envelope) is forwarded.       |
| `idempotency_conflict`  | Same `client_request_id` already in flight; the in-progress call wins, the second returns this code with `meta.in_flight: true`. (Completed responses replay; only concurrent duplicates conflict.) |
| `internal_error`        | Anything else (DB error before tx, marshal failure, etc.). Message redacted.          |

Guest-supplied error codes are forwarded verbatim. By convention guests
SHOULD use `snake_case` so they do not collide with the kernel's reserved
set; the kernel does not enforce this.

## Status code reference

| Code | When                                                                                            |
| ---- | ----------------------------------------------------------------------------------------------- |
| 200  | Success.                                                                                        |
| 400  | `invalid_id`, `invalid_input`, malformed JSON body.                                             |
| 401  | UserResolver returned nil.                                                                      |
| 403  | `forbidden` (capability check).                                                                 |
| 404  | `model_not_found`, `action_not_found`, `record_not_found`.                                      |
| 409  | `invalid_state`, `serialization_failure` (retryable), `idempotency_conflict`.                   |
| 422  | Guest returned `{success:false}` with a non-kernel error code (action declined for business reasons). |
| 499  | `canceled` (client-aborted; or framework's equivalent).                                         |
| 500  | `runtime_error`, `memory_exhausted`, `internal_error`.                                          |
| 502  | `webhook_unreachable`.                                                                          |
| 503  | `bus_unavailable`.                                                                              |
| 504  | `timeout`.                                                                                      |

Mapping lives in `dynamic/handler.go:handleError` (extended for the new
codes). `503` is preferred over `500` for `bus_unavailable` because the
condition is a deployment-time misconfig, recoverable by restarting the
host with a wired bus — same rationale `wasm-abi.md` §12.4 uses.

## Idempotency

The endpoint integrates with the existing `idempotency.Middleware`
([`idempotency/middleware.go`](../idempotency/middleware.go)). Apps wire
it under `MountOpts.MutationMiddleware` so action POSTs flow through the
same replay logic as `Create` / `Import`.

Idempotency-key resolution order:

1. `Idempotency-Key` request header (RFC 8594-style; preferred).
2. Body's `client_request_id` (explicit body shape only).
3. `Trigger.IdempotencyKey` evaluated as a JSONPath over the validated
   payload (e.g. `"$.payload.client_request_id"`). Missing / empty result
   leaves the request non-idempotent.

The resolved key is forwarded to the trigger as
`ctx.idempotency_key` (see WASM envelope).

When the same key is observed within the configured TTL (default 24h):

- If a completed response is cached → replay it verbatim with
  `meta.idempotent: true`. HTTP status replayed too.
- If a request with the same key is still in flight → return
  `409 idempotency_conflict` with `meta.in_flight: true`. The caller is
  expected to back off and retry against the cached completion.

`Trigger.IdempotencyKey` is evaluated *after* body validation so a
malformed payload never pollutes the idempotency table.

## Audit

When `Trigger.Audited == true`, the kernel writes a row to
`metacore_audit` after the trigger returns (whether commit or rollback):

| Column         | Value                                                                            |
| -------------- | -------------------------------------------------------------------------------- |
| `id`           | UUID; surfaced in the response as `meta.audit_id`.                               |
| `created_at`   | Server time at write.                                                            |
| `org_id`       | `principal.org_id`.                                                              |
| `user_id`      | `principal.user_id`.                                                             |
| `action`       | `<model>::<action>`.                                                             |
| `payload_hash` | SHA-256 hex of the canonical JSON of the validated payload. Bodies are not stored verbatim. |
| `status`       | `committed`, `rolled_back`, `webhook_error`, `runtime_error`, `timeout`, `memory_exhausted`, `serialization_failure`, `canceled`. |
| `latency_ms`   | Wall time from request start to envelope write.                                  |
| `tx_id`        | Action transaction id when `RunInTx`; NULL otherwise.                            |
| `request_id`   | Kernel request id.                                                               |

The audit row is best-effort: a failure to write it does not fail the
action (the trigger has already been decided by the time we audit). When
the audit write itself fails the kernel logs but does not surface the
error.

## Capabilities

`Trigger.RequiredCapability` is enforced by the kernel before the trigger
runs. The check is delegated to the same `security.Enforcer` used by the
WASM host imports; modes (`ModeEnforce` / `ModeShadow`) apply identically
— in shadow mode the violation is logged but the action proceeds.

The capability is declared on the principal (user/role), not on the addon.
This is distinct from the WASM ABI's `db:write` / `event:emit` capabilities
which are declared on the addon manifest. v1 keeps the two surfaces
separate; a future iteration may compose them (e.g. "user has
`tickets.escalate` AND addon declares `db:write addon_tickets.*`").

## Limits

| Knob                            | Default          | Configurable via                                       |
| ------------------------------- | ---------------- | ------------------------------------------------------ |
| Max request body size           | 1 MiB            | host-side fiber config; mirrors `Create`.              |
| Max validated payload size      | 256 KiB          | host-side `dynamic` config; payload above is rejected pre-trigger. |
| Per-call wall time              | 30 s             | `Trigger.TimeoutMs` (lower wins, bounded by `BackendSpec.TimeoutMs` for wasm). |
| Idempotency cache TTL           | 24 h             | host-side idempotency config.                          |
| Max concurrent in-flight per (installation, action, idem_key) | 1 | enforced by the idempotency store; second conflicts. |

The 30 s default is generous because actions can chain remote calls; UI
SLAs typically clamp `Trigger.TimeoutMs` to 5–10 s.

## Out of scope for v1

Deliberately deferred — each lands as its own proposal:

- **Bulk actions.** `POST /dynamic/:model/action/:key` with a list of ids.
  v1 is per-row only; bulk needs its own envelope (per-row results, partial
  failure semantics, transaction-per-row vs single tx).
- **Async / queued actions.** `Trigger.Async == true` is accepted by the
  validator and currently rejected at dispatch (`501 not_implemented`).
  v1.x adds a `metacore_jobs` enqueue path.
- **`Trigger.CompensateFunction` (sagas).** Marked v2 in the audit
  roadmap; requires an outbox table.
- **Streaming responses.** Long-running actions cannot stream progress in
  v1; the response is single-shot. SSE / chunked streaming is a separate
  endpoint shape.
- **`dry_run: true`.** Reserved in the explicit body shape but not yet
  implemented; v1.1 will run the trigger inside a tx that always rolls
  back, returning the would-be response.
- **`Trigger.RateLimit`.** Field accepted by the validator but enforced
  best-effort only in v1 (no distributed counter); v1.x adds the proper
  per-(user, org, action) sliding window.
- **`Trigger.Phase == "before"` with a kernel-owned mutation.** v1 has no
  kernel-owned writes; the phase distinction is preserved for forward
  compatibility but functionally collapses to `instead_of`. v1.x adds
  per-action stamps (`last_action_at`, `last_action_by`) that make
  `before` vs `after` observable.
- **Cross-installation actions.** v1 dispatches only to the installation
  that owns the row's model. Routing to a different installation (e.g.
  "ask the billing addon to refund this orders row") is a separate ABI.

## Tests (proposed coverage)

Functional matrix to exercise once the endpoint lands:

- **404 model_not_found.** Unknown `:model` → `404`, no row read.
- **404 record_not_found.** Known model, unknown id, or id from another
  org → `404`, no trigger dispatched.
- **404 action_not_found.** Known row, unknown `:key` → `404`.
- **400 invalid_id.** Non-UUID `:id` → `400`.
- **400 invalid_input.** Body missing a required `Field` → `400`.
- **409 invalid_state.** `RequiresState=["open"]`, row is `resolved` →
  `409`, no trigger dispatched.
- **403 forbidden.** Principal lacks `Trigger.RequiredCapability` →
  `403`, no trigger dispatched.
- **wasm + RunInTx happy path.** Guest emits `db_exec INSERT INTO
  addon_tickets.audit_log` and returns `{success:true, data:{ticket_id,
  queue:"tier-2"}}` → `200`, row in audit_log committed, response
  includes `data.result.queue == "tier-2"` and `meta.tx_id`.
- **wasm + RunInTx rollback on success:false.** Guest does an
  `db_exec UPDATE` then returns `{success:false, error:{code:"declined"}}`
  → `422`, the update is rolled back (verified by re-querying outside
  the tx), envelope's `meta.rolled_back == true`.
- **wasm + RunInTx rollback on panic.** Guest panics after a successful
  `db_exec` → `500 runtime_error`, rollback verified, no row visible.
- **wasm + RunInTx rollback on timeout.** Guest sleeps past
  `TimeoutMs` → `504 timeout`, rollback verified.
- **wasm + RunInTx serialization conflict.** Two concurrent actions on
  the same row trigger SQLSTATE 40001 → loser sees `409
  serialization_failure`, `meta.retryable: true`.
- **wasm without RunInTx.** Guest calls `db_exec` → guest receives
  `no_active_tx` error envelope; guest forwards `{success:false}` →
  `422`, no kernel rollback (no tx existed), `meta.rolled_back` absent.
- **wasm ReturnsRow=true.** After commit, response includes `data.row`
  reflecting the guest's mutations; verify the row is read inside the
  tx (no dirty-read race).
- **webhook legacy path.** No `Trigger`, `manifest.Hooks` resolves →
  signed POST, response forwarded into `data` with backwards-compat
  wrapping when the remote returns raw JSON.
- **webhook unreachable.** Trigger.URL points to a closed port →
  `502 webhook_unreachable`, no rollback (no tx).
- **event publish.** `Trigger.Type="event"`, one subscriber → `200
  {success:true, data:{event, subscribers:1}}`.
- **none.** `Trigger.Type="none"` → `200 {success:true, data:{row},
  meta:{no_op:true}}`, no dispatch.
- **idempotency replay.** Same `Idempotency-Key` within TTL → second
  call replays the cached response with `meta.idempotent:true` and the
  trigger is NOT re-invoked.
- **idempotency conflict.** Two concurrent calls with the same key →
  first wins, second gets `409 idempotency_conflict`,
  `meta.in_flight:true`.
- **audit row written on commit.** `Trigger.Audited=true`, success path
  → `metacore_audit` has a row with `status=committed`,
  `payload_hash` matches.
- **audit row written on rollback.** Guest returns `success:false` →
  `metacore_audit` has `status=rolled_back`, same `payload_hash`.
- **validator: wasm without Backend.** Manifest has `Trigger.Type=wasm`
  but no `Backend` block → load-time validation error.
- **validator: wasm function not exported.** `Trigger.Function` not in
  `Backend.Exports` → load-time validation error.
- **validator: RunInTx + Async.** Both `true` → load-time validation error.
- **validator: RunInTx + Phase=after.** → load-time validation error.

Performance and load tests are out of scope for the v1 spec but the
implementation PR will include a `bench_test.go` measuring the per-call
overhead (target: ≤ 5 ms above the bare `Host.InvokeInTx` cost so the
endpoint does not become the bottleneck of the action path).

---

See also: [`dynamic-api.md`](dynamic-api.md), [`wasm-abi.md`](wasm-abi.md),
[`docs/audits/2026-05-04-action-trigger-gap.md`](audits/2026-05-04-action-trigger-gap.md),
[`docs/audits/2026-05-04-host-functions-gap.md`](audits/2026-05-04-host-functions-gap.md),
[`permissions.md`](permissions.md).
