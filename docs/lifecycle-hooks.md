# Lifecycle Hooks (kernel ≥ v0.11.0)

`manifest.lifecycle_hooks` is a map an addon declares to ask the kernel to
dispatch custom logic at well-known lifecycle and CRUD events. Previous to
v0.11.0 the field validated but **no consumer read it** — addons could
declare hooks that never fired. v0.11.0 makes them functional.

The contract is additive: an addon that does not declare
`lifecycle_hooks` keeps the pre-v0.11.0 behaviour. Hosts opt in to the
runner via `host.AppConfig.EnableLifecycleHooks = true` (or by wiring
`installer.Installer.HookRunner` directly).

## 1. Supported events

| Event              | Source                  | Veto on error? |
|--------------------|-------------------------|----------------|
| `install`          | `installer.Install`     | yes (aborts install) |
| `uninstall`        | `installer.Uninstall`   | yes (aborts uninstall) |
| `enable`           | `installer.Enable`      | yes |
| `disable`          | `installer.Disable`     | yes |
| `upgrade`          | `installer.Upgrade`     | yes (before only — after errors are logged) |
| `before_create`    | `dynamic.Service.Create` | yes (aborts the mutation) |
| `after_create`     | `dynamic.Service.Create` | no (error is logged) |
| `before_update`    | `dynamic.Service.Update` | yes |
| `after_update`     | `dynamic.Service.Update` | no |
| `before_delete`    | `dynamic.Service.Delete` | yes |
| `after_delete`     | `dynamic.Service.Delete` | no |

Lifecycle transitions and `before_*` events block the calling operation
until the hook returns. `after_*` events run after the row is persisted;
errors are logged and swallowed (a flaky notification hook can't strand a
successfully-committed row).

## 2. Declaration shape

```jsonc
{
  "lifecycle_hooks": {
    "install": [
      {
        "target": { "type": "wasm", "function": "on_install" }
      }
    ],
    "after_create": [
      {
        "target": { "type": "webhook", "url": "https://addon.example/hooks/created" },
        "async":  true,
        "priority": 0
      },
      {
        "target": { "type": "webhook", "url": "https://addon.example/hooks/audit" },
        "priority": 10
      }
    ]
  }
}
```

Per-`HookDef` fields:

| Field      | Type                   | Notes |
|------------|------------------------|-------|
| `event`    | string (optional)      | Mirrors the map key for self-documentation. Validation rejects a mismatch. |
| `target.type` | `"wasm" \| "webhook" \| "prompt"` | Required. `prompt` validates but currently no-ops. |
| `target.function` | string | Required for `type=wasm`; MUST appear in `backend.exports`. |
| `target.url` | string | Required for `type=webhook`. |
| `target.prompt` | string | Required for `type=prompt`. |
| `priority` | int | Lower numbers fire first within the same event. Defaults to 0. |
| `async`    | bool | Allowed only on `after_*` events — `before_*` and lifecycle events veto the operation on error, so async would silently drop the contract. |

## 3. Dispatch model

The kernel runs one `lifecycle.HookRunner` per process. Hosts register a
`lifecycle.HookDispatcher` per `target.type` — kernel-bundled examples:

| `target.type` | Dispatcher source |
|---------------|-------------------|
| `wasm`        | a thin adapter that calls `runtime/wasm.Host.Invoke` with the hook payload |
| `webhook`     | a thin adapter that calls `security.WebhookDispatcher.Dispatch` |
| `prompt`      | host-provided (e.g. a conversational hub) |

Custom hosts can swap or extend the table by calling
`HookRunner.Register("<type>", dispatcher)`.

### Order of execution

For each fired event the runner:

1. Reads `manifest.lifecycle_hooks[<event>]`.
2. Sorts entries by `priority` (ascending; insertion-sort, stable for
   ties).
3. Dispatches sequentially. `before_*` and lifecycle events abort the
   chain on the first error. `after_*` events keep going on errors and
   log each failure.
4. `async: true` after-events fire-and-forget on a goroutine; the
   commit path returns immediately.

### Payload shape

Lifecycle transitions receive:

```json
{
  "event": "install",
  "addon_key": "tickets",
  "org_id": "ab2f...",
  "version": "1.2.0"
}
```

The `upgrade` event carries an enriched payload because the addon often
needs both the source and target version to know which migration path
applies. Two dispatches happen per upgrade — `phase: "before"` runs before
any schema or bundle work; `phase: "after"` runs once the new version has
committed and includes the count of newly-applied migrations:

```json
// before — fired before any schema/bundle mutation; non-nil error aborts.
{
  "event": "upgrade",
  "addon_key": "tickets",
  "org_id": "ab2f...",
  "from_version": "1.0.0",
  "to_version": "1.1.0",
  "phase": "before"
}

// after — fired post-commit. Errors are logged and swallowed (the upgrade
// has already persisted and DDL rollback is unsafe). `migrations_applied`
// counts files in the new bundle that were NOT already recorded in
// `metacore_addon_migrations`; legacy / shared files are skipped by
// dynamic.Apply and do not contribute to the count.
{
  "event": "upgrade",
  "addon_key": "tickets",
  "org_id": "ab2f...",
  "from_version": "1.0.0",
  "to_version": "1.1.0",
  "phase": "after",
  "migrations_applied": 2
}
```

Guard rails enforced by `installer.Upgrade` BEFORE the first hook fires:

| Sentinel error                | Cause                                                                   | HTTP code |
|-------------------------------|-------------------------------------------------------------------------|-----------|
| `installer.ErrNotInstalled`   | `(org, addon_key)` has no row in `metacore_installations`               | 404       |
| `installer.ErrCannotDowngrade`| `to_version` sorts strictly lower than the installed version (semver)   | 409       |
| `installer.ErrSameVersionUpgrade` | `to_version` equals the installed version                           | 409       |

CRUD events receive:

```json
{
  "event": "after_create",
  "model": "tickets",
  "record": { ... persisted row ... }
}
```

For `update` the payload also carries `id` and `input`; for `delete` it
carries `id`. The shape is stable across kernel versions; new optional
fields may be added.

## 4. Timeouts

WASM hooks honour `backend.timeout_ms` (default 10s). Webhook hooks honour
the dispatcher's default (`security.WebhookDispatcher`, currently 20s). A
timed-out hook is treated as an error: aborts before-events, logs
after-events.

## 5. Wiring (host integration)

A consuming app turns the runner on via:

```go
app := host.NewApp(host.AppConfig{
    DB:                   db,
    JWTSecret:            secret,
    EnableLifecycleHooks: true,
})

// Register dispatchers. These imports are host-local because the runtime
// instance is shared with the rest of the addon pipeline.
app.HookRunner.Register(lifecycle.HookTargetWasm,    myWasmDispatcher)
app.HookRunner.Register(lifecycle.HookTargetWebhook, mySignedWebhookDispatcher)

// Wire installer so install/enable/disable/uninstall fire their hooks
// and CRUD hooks land in dynamic.Service's HookRegistry.
h, _ := host.New(host.Config{
    DB:            db,
    KernelVersion: "2.0.0",
    HookRunner:    app.HookRunner,
    DynamicHooks:  app.DynamicHooks,
})
```

Hosts that don't enable the runner keep the previous behaviour: declared
hooks are validated but never dispatched.

## 6. Reinstall idempotency

`installer.Install` calls `dynamic.HookRegistry.UnregisterAddon(addonKey)`
before registering the new manifest's CRUD hooks, so re-installing an
addon never doubles up. Lifecycle events do not need this guard — each
transition fires exactly once.

## 7. Error semantics summary

| Event family    | Hook error           | Operation outcome |
|-----------------|----------------------|-------------------|
| lifecycle (`install`/`enable`/`disable`/`uninstall`/`upgrade`) | Returned | Operation aborts |
| `before_create`/`before_update`/`before_delete`              | Returned | Mutation aborts |
| `after_create`/`after_update`/`after_delete`                 | Logged   | Row stays committed |

`async: true` on after-events: errors only ever surface in the host's
structured logs (the calling goroutine has long since returned).

## 8. Backfills — the sweep a lifecycle hook cannot be

A lifecycle hook is *one* dispatch. That is the right shape for a seed
("create the three default statuses") and the wrong shape for a
**backfill** — materializing a projection over rows that already existed
when the addon was installed.

Two hard limits make the single-dispatch shape unworkable for a sweep,
and neither is fixable inside the guest:

- **The guest cannot enumerate its own backlog.** `data_query` is capped
  at 200 rows, filters by equality only, and has no offset or cursor
  (`docs/wasm-abi.md` §15.2). A "regenerate everything" export cannot see
  past the first page, no matter how it is written.
- **The guest cannot finish inside one deadline.** Handlers run under
  `backend.timeout_ms` (default 10s). A sweep over thousands of keys,
  each doing several queries and a batch write, exhausts that long before
  it is done — and a partial run leaves no record of where it stopped.

`manifest.backfills` inverts the loop: the **host** enumerates, the guest
handles one key at a time.

```jsonc
{
  "backfills": [
    {
      "key": "customer_statements",
      "on": ["install", "upgrade"],          // omitted = both
      "source": { "table": "invoices", "distinct": "customer_id" },
      "do": "wasm:backfill_party",
      "arg": "party_id",
      "with": { "party_type": "customer" }
    }
  ]
}
```

At each declared transition the host runs ONE org-scoped
`SELECT DISTINCT <source.distinct> FROM <source.table>` (skipping NULLs
and soft-deleted rows, plus any `source.where` equality predicates) and
dispatches `do` once per distinct value:

```json
{ "backfill": "customer_statements", "party_id": "…", "party_type": "customer" }
```

Each dispatch is an independent invocation with its own deadline, so
per-key work stays inside the normal handler budget and one failing key
isolates from the rest.

| Field | Notes |
|---|---|
| `key` | Unique within the addon. Identifies the sweep in the payload and in host progress records. |
| `on` | `install` and/or `upgrade`. **Omitted means both** — an install-only default would leave every already-installed org stale forever, which is the failure this primitive exists to fix. |
| `source.table` / `source.distinct` | Logical unqualified table and the column whose distinct values form the key set. |
| `source.where` | Optional equality-only predicates, mirroring `data_query` so a declaration cannot express a filter the guest could not. |
| `do` | Same reference grammar as `Schedule.Do`: `wasm:<export>` \| `webhook:<key>` \| `compiled:<fn>`. |
| `arg` | Payload field the value is passed as. Defaults to `id`. |
| `with` | Constants merged into every payload, so one export serves several sweeps. May not set `arg` or the reserved `backfill` key — validation rejects the shadowing rather than picking a winner. |

**The handler must be idempotent.** Dispatch is at-least-once and
unordered: the host may re-run a sweep that was interrupted, and makes no
promise about the order keys arrive in. In practice the natural target is
the export an addon already subscribes to its own domain events with — the
incremental path that is proven in production — which is idempotent by
construction.

Backfills are declared here and in `manifest.Manifest.Backfills` (the
host projection); **running** them is the host's job, next to where it
already runs install/upgrade. The kernel does not schedule them itself,
the same division schedules use (kernel declares, host's runtime fires).

## 9. Reserved / future work

- `target.type = "prompt"`: declared and validated; runtime delegation
  awaits a kernel-bundled LLM dispatcher. Hosts can register a custom
  prompt dispatcher today and the runner picks it up via the standard
  `HookRunner.Register` API.
