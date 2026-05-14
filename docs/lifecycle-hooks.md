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
| `upgrade`          | reserved for future use | yes |
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

## 8. Reserved / future work

- `upgrade` event: the constant exists and is fired by future Installer
  upgrade flows; addons can declare hooks today and they will be
  validated, but the kernel only fires `install`/`enable`/`disable`/
  `uninstall` until the upgrade path lands.
- `target.type = "prompt"`: declared and validated; runtime delegation
  awaits a kernel-bundled LLM dispatcher. Hosts can register a custom
  prompt dispatcher today and the runner picks it up via the standard
  `HookRunner.Register` API.
