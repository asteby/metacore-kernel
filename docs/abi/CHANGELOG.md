# Metacore ABI Changelog

This changelog tracks the **ABI v1** contract — the union of the manifest
schema, the bundle format, the WASM guest ABI and the `metacore_host`
import surface. It is intentionally separate from the kernel
[`CHANGELOG.md`](../../CHANGELOG.md), which tracks the embedding API and
the host-facing services.

Read [`v1.md`](./v1.md) for the full normative contract.

## v1.0.0 — 2026-05-13

**First frozen revision.** Anchored on kernel `v0.10.1`. Anything listed
below is **stable**: every kernel minor in the v1 series will keep
honouring it without addon-side changes. Breaking changes require a v2
bump with an overlap period.

### Manifest schema (frozen)

- Top-level `Manifest` shape (`manifest/manifest.go`): identity fields
  (`key`, `name`, `description`, `version`, `category`, `kernel`,
  `author`, `website`, `license`, `readme`, `screenshots`, `features`,
  `price`), icon triplet (`icon` legacy + `icon_type` / `icon_slug` /
  `icon_color`).
- `tenant_isolation`: closed set `""` (= `shared`), `shared`,
  `schema-per-tenant`, `database-per-tenant` (reserved).
- `model_definitions[]`: `table_name`, `model_key`, `label`,
  `org_scoped`, `soft_delete`, `columns[]`, `relations[]`, `table`,
  `modal`.
- `ColumnDef` (in `columns[]`): `name`, `type`, `size`, `required`,
  `index`, `unique`, `default` (whitelisted literals + builtins),
  `ref`, `visibility` (`""|all|table|modal|list`), `searchable`,
  `validation` (`regex|min|max|custom`), `widget` (closed set of 19
  slugs).
- `RelationDef`: closed set of `kind` values
  (`one_to_many|many_to_many|belongs_to`) with the field-presence
  rules in `v1.md` § 2.4.2.
- `navigation[]` / `NavGroup` / `NavItem`.
- `frontend`: `entry`, `format` (`federation|script|""`), `expose`,
  `container`, `integrity`, `layout` (`""|shell|immersive`).
- `backend`: `runtime` (`webhook|wasm|binary` — `binary` reserved),
  `entry`, `url`, `exports`, `memory_limit_mb` (default 64),
  `timeout_ms` (default 10 000).
- `capabilities[]`: closed kind set
  `db:read|db:write|http:fetch|event:emit|event:subscribe` with the
  target-shape rules in `v1.md` § 2.8. Bare `*` rejected for `db:*`
  and TLD-less for `http:fetch`.
- `permissions[]`: legacy declarative form, preserved for retro-compat.
  No new fields accepted; will be removed in v2.
- `hooks` map: `"<model>::<action>"` → URL. When
  `backend.runtime=wasm`, every `<action>` half MUST appear in
  `backend.exports[]` (enforced by `manifest.Validate`).
- `actions` map: `model` → `[]ActionDef`. `ActionDef` carries optional
  `trigger` (`ActionTrigger`).
- `ActionTrigger`: closed `type` set (`wasm|webhook|noop`) with the
  field-presence rules in `v1.md` § 7.
- `tools[]`: LLM-facing actions for conversational hosts (`bridge/tools.go`).
- `settings[]`: per-installation configurable values with `secret`
  flag.
- `lifecycle_hooks` map: declared and validated, RESERVED in v1.0
  (compiled addons receive lifecycle through the `lifecycle.Addon`
  Go interface; WASM addons do not receive lifecycle hooks).
- `events[]`: declared and validated, RESERVED in v1.0 (advisory; the
  authoritative gate is `capabilities[].event:emit`).
- `signature`: Ed25519 over the raw bundle bytes with hex-encoded
  digest + value. Per-file SHA-256 checksums (optional).
- `i18n` map: `<locale>` → `<key>` → translated string.

### Manifest validation (frozen)

- Identifier alphabets (`v1.md` § 2.18): addon key, model / table,
  column, wasm export, custom validator slug, org-ref validator,
  event name.
- `kernel` semver range check against the running kernel version.
- Column `default` literal whitelist (numeric, single-quoted string,
  `now()`, `gen_random_uuid()`, `uuid_generate_v4()`,
  `current_timestamp`, `true`, `false`, `null`).
- Relation field-presence (no `pivot` for `one_to_many` /
  `belongs_to`; required for `many_to_many`).
- Trigger field-presence (`export` required + listed for `wasm`;
  forbidden + `run_in_tx=false` for `webhook` / `noop`).
- Action triggers cross-checked against `backend.exports[]`.

### Bundle format (frozen)

- `tar.gz` layout: `manifest.json` (required), `migrations/<v>.sql`
  (optional, applied in lexicographic order), `frontend/*` (opaque),
  `backend/*` (opaque; carries `backend/backend.wasm` for
  `runtime=wasm`), `README.md` (optional).
- Reproducible writes: deterministic entry order, fixed
  `mtime = unix 0`, PAX format.
- Path safety: no absolute, no `..`, no leading `/`.
- Decompression cap (default 64 MiB), enforced over bytes actually
  read.
- Per-file SHA-256 captured in `Bundle.EntryDigests`; the security
  verifier rejects both declared mismatches and undeclared extras
  (except `manifest.json`, which is covered by the global Ed25519).

### WASM ABI (frozen)

- Required guest exports: `memory`, `alloc(size i32) -> i32`,
  `<export>(ptr i32, len i32) -> i64` for each entry in
  `backend.exports[]`.
- Pointer/length encoding: `(uint64(ptr) << 32) | uint64(len)`. `0` is
  reserved for empty success on guest exports.
- Two host entry points: `Host.Invoke` (no surrounding transaction)
  and `Host.InvokeInTx` (passes a `*gorm.DB` transaction for action
  triggers with `run_in_tx=true`).
- Per-installation instance caching; module-level globals persist
  within an installation, reset on `Host.Load`.
- Synchronous, serialised invocation per `(addonKey, installation)`.
- `backend.timeout_ms` (default 10 000) bounds wall-clock per
  invocation; `db_query` / `db_exec` impose a 5 s per-call deadline
  on top.

### Host imports (frozen — module `metacore_host`)

- `log(msgPtr, msgLen) -> void` — structured log line tagged with
  addon key + installation.
- `env_get(keyPtr, keyLen) -> i64` — per-installation setting
  lookup, returns `0` for missing.
- `http_fetch(urlPtr, urlLen, methPtr, methLen, bodyPtr, bodyLen) -> i64`
  — outbound HTTP with SSRF guard, 30 s request cap, 8 MiB response
  cap, JSON envelope `{status, body}` on success, JSON error envelope
  on failure.
- `db_query(sqlPtr, sqlLen, argsPtr, argsLen) -> i64` — single-
  statement read; `SET LOCAL search_path TO addon_<key>, public`;
  always returns a JSON envelope. Defined error codes: `invalid_sql`,
  `arg_decode`, `forbidden`, `row_limit_exceeded`, `db_error`,
  `db_unavailable`. Limits: 16 KiB SQL, 64 args, 5 s deadline,
  10 000 rows, 8 MiB response. Argument encoding includes the
  `$uuid` / `$ts` / `$bytes` markers.
- `db_exec(sqlPtr, sqlLen, argsPtr, argsLen) -> i64` — single-
  statement mutation; reuses the action handler's open `*gorm.DB`
  transaction when entered via `Host.InvokeInTx`. Defined error
  codes: `invalid_sql`, `arg_decode`, `forbidden`, `db_error`,
  `db_unavailable`. Returns `{success, data:{rowsAffected},
  meta:{schema, durationMs}}`. `RETURNING` rows NOT projected in v1.0
  (audit-known, additive expansion path in v1.x).
- `event_emit(eventPtr, eventLen, payloadPtr, payloadLen) -> i64` —
  publishes through `events.Bus`. Returns `0` on success, JSON error
  envelope on failure. Event-name cap 256 B, payload cap 256 KiB.
  Defined error codes: `bus_unavailable`, `invalid_event`,
  `payload_too_large`, `invalid_payload`, `forbidden`. `orgID` is
  taken from the per-invocation context bag (defaults to `uuid.Nil`
  until callers thread it through — audit-known).

### Lifecycle (frozen)

- Compiled addon interface `lifecycle.Addon` (`OnInstall` /
  `OnUninstall` / `OnEnable` / `OnDisable`) and optional
  `lifecycle.Bootstrapper.Boot(*BootContext)`.
- Lifecycle hooks for WASM addons via the manifest's
  `lifecycle_hooks` map are reserved (declared & validated but not
  yet routed at runtime).

### Event bus (frozen)

- `events.Bus.Publish(ctx, addonKey, event, orgID, payload)` —
  synchronous fan-out.
- Subscribe wildcards: trailing `.*` only.
- Kernel-owned events: `<addonKey>.<model>.<action>` with the
  `dynamic.CanonicalEvent` payload shape (`{id, before?, after?}`).
- Capability check runs through `Enforcer.CheckCapability` with
  `kind = "event:emit" | "event:subscribe"`; the kernel itself
  (`addonKey == "kernel"`) bypasses the check.

### Capability model (frozen)

- Closed kind set (§ 2.8).
- Two enforcer modes: `ModeShadow` (log only) and `ModeEnforce`,
  toggled at boot via `METACORE_ENFORCE`.
- Trailing-dot-star wildcards honoured on every kind's target.

### Audit notes documented in v1.0 (frozen-as-is)

The following gaps between code and the proposal text are part of v1.0
and will NOT be silently fixed inside v1 — they have explicit resolution
paths documented in `v1.md` § 12:

1. `manifest.lifecycle_hooks` is reserved (not yet routed).
2. `manifest.events` is reserved (advisory).
3. `backend.runtime="binary"` is reserved (no implementation).
4. `event_emit` returns `0` on success (richer envelope is additive in
   v1.x).
5. `event_emit` `orgID` defaults to `uuid.Nil` until `Host.Invoke` is
   extended to accept it.
6. `db_query` does not walk an AST in v1.0; cross-schema gating relies
   on `SET LOCAL search_path` + the single self-schema capability check.
7. `db_exec` does not project `RETURNING` rows in v1.0.

Each is an opportunity for an additive v1.x landing.
