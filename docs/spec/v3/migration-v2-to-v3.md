# Migrating from Module Contract v2 to v3

This document specifies the field-by-field mapping the kernel uses to read v2
manifests through the v3 in-memory model, and the policy timeline for
removing v2 support.

## Policy

| Kernel line | v2 behaviour                              | v3 behaviour                        |
| ----------- | ----------------------------------------- | ----------------------------------- |
| **3.x**     | **Dual-read.** Both v2 and v3 manifests install. v2 is up-converted by the installer to the v3 in-memory shape; downstream code sees v3 only. | First-class. |
| **4.x**     | **Removed.** Manifests without `apiVersion: asteby.com/v3` are rejected at install time. | First-class. |

Foundation addons MUST publish a v3 manifest before the 4.x release train
opens. Third-party addons get one minor release of warning before 4.0.

A manifest is detected as v3 when it carries the top-level
`apiVersion: "asteby.com/v3"`. Anything else (including a missing
`apiVersion`) is treated as v2.

## Field-by-field mapping

| v2 path                               | v3 path                                                   | Notes |
| ------------------------------------- | --------------------------------------------------------- | ----- |
| `key`                                 | `metadata.key`                                            | identical semantics. |
| `name`                                | `metadata.name`                                           | identical. |
| `description`                         | `metadata.description`                                    | identical. |
| `version`                             | `metadata.version`                                        | identical. |
| `category`                            | `metadata.category`                                       | identical. |
| `icon`, `icon_type`, `icon_slug`, `icon_color` | `metadata.icon = {type, slug, color}`             | the v2 triplet is folded into a single object. Legacy `icon` string is mapped to `metadata.icon.slug` with `type = "lucide"`. |
| `kernel`                              | `compatibility.requires[]` entry with `key: "kernel"`     | v2 string becomes a Requirement with the same range. |
| `requires[]`                          | `compatibility.requires[]`                                | each `{key,label}` becomes `{key, version: "*"}`. Authors should tighten the range during the upgrade. |
| `models[]`                            | `models[]`                                                | v2 `models` was just a `{key,label}` declaration; v3 demands a full `Model` with columns. The installer keeps a thin "name-only" Model for backwards compat during 3.x. |
| `tenant_isolation`                    | `tenancy.isolation`                                       | v2 values `"shared"`, `"schema-per-tenant"`, `"database-per-tenant"` map to v3 `"shared"`, `"schema"`, `"database"`. v2 default empty string → v3 `"shared"`. |
| (implicit `organization_id` column)   | `tenancy.rls_column`                                      | v3 makes the column name explicit; default `"organization_id"`. |
| `navigation[]`                        | `contributions.navigation[]`                              | identical shape. |
| `extensions[]`                        | `models[].extensions[]` (per target model)                | v2 flat array is regrouped under the owning model. |
| `settings[]`                          | `settings[]`                                              | identical. |
| `hooks{}`                             | `contributions.subscriptions[]`                           | each `event => path` becomes a subscription `{event, handler:{type:"webhook", url:path}}`. |
| `actions{}`                           | `contributions.actions[]`                                 | the v2 map keyed by model is flattened; `target_model` carries the key. |
| `tools[]`                             | `contributions.tools[]`                                   | identical. |
| `model_definitions[]`                 | `models[]`                                                | merged into the unified Model definition. |
| `events[]`                            | `extension_points.events[]`                               | each event name becomes `{name}`. Authors should add a `payload_schema` during the upgrade. |
| `lifecycle_hooks{}`                   | `lifecycle.{install,enable,...}` + `contributions.subscriptions[]` | install/uninstall/enable/disable/upgrade hooks map onto `lifecycle.*`; CRUD before/after hooks become subscriptions on the corresponding model events. |
| `i18n{}`                              | `i18n.bundles[]`                                          | inline maps in v2 become file-path references in v3. The kernel 3.x dual-read inlines them in a synthesised default bundle. |
| `frontend{}`                          | (kernel-internal; not part of v3 contract surface)        | the federation entry stays in the bundle manifest, not the contract manifest. |
| `backend{}`                           | (kernel-internal)                                         | same: runtime selection lives in the bundle manifest. |
| `capabilities[]`                      | `capabilities[]`                                          | the closed set is unchanged for the kinds that existed in v2; `cron:register`, `queue:produce`, `queue:consume`, `file-storage:write`, `time:wallclock` are new in v3. |
| `permissions[]` (legacy)              | `rbac.permissions[]`                                      | the v2 model/scope shape is rebuilt as `{key,label,description}`. Roles are new in v3. |
| `signature{}`                         | `signature{}`                                             | shape is incompatible: v2 used `algorithm,key_id,value`; v3 mandates ed25519 + `signed_at`. The v2 → v3 up-converter rejects v2 signatures and requires re-signing. |
| `author,website,license,readme,screenshots,features,price` | `metadata.*`                            | folded under metadata. `price` is removed; pricing now lives in `billing.metered_events[].revenue_share` and the marketplace catalog. |

## New in v3 (no v2 equivalent)

- `compatibility.requires[].optional` + `reason`
- `contributions.slots[]` and `extension_points.slot_kinds[]` — typed UI
  extension surfaces. v2 addons had to ship their own ad-hoc registration.
- `extension_points.model_extensions_accepted[]` — explicit opt-in.
- `models[].foreign_keys[].policy` — logical vs physical FK choice.
- `lifecycle.upgrade[]` ladder with semver-range matched steps.
- `rbac.roles[]` — first-class roles.
- `billing.metered_events[]` — manifest-declared metering.
- `kind: "Preset" | "Theme" | "ConnectorPack"` and their dedicated blocks.

## Removed in v3 (no longer in the contract)

- `Hooks` map keyed by event name. Use `contributions.subscriptions[]`.
- `LifecycleHooks` map keyed by event name. Use `lifecycle.*` (lifecycle
  transitions) and `contributions.subscriptions[]` (CRUD events).
- Free-form `Events []string`. Use `extension_points.events[]` with a typed
  payload schema, gated by the `event:emit` capability.
- `Frontend`/`Backend` blocks inside the contract manifest. They survive in
  the *bundle* manifest (`bundle.json`) consumed by the loader; they are
  not part of the inter-addon contract.

## Up-converter behaviour (kernel 3.x)

When the kernel reads a manifest without `apiVersion`:

1. Parse with the v2 schema.
2. Map every v2 field through the table above.
3. Synthesise:
   - a `compatibility.requires` entry for the kernel using the v2 `kernel`
     range (or `>=3.0.0 <4.0.0` if absent),
   - a `tenancy` block with `isolation` derived from `tenant_isolation` and
     `rls_column = "organization_id"`,
   - a `signature: null` (v3 signatures require re-signing).
4. Run the v3 schema validator on the up-converted document. Failures are
   reported with the v2 source path so authors get accurate errors.

The up-converter is part of `manifest/v3` and reused by the installer, the
CLI `migrate` subcommand and the marketplace ingest pipeline so all three
agree on the same semantics.

## Migration checklist for addon authors

1. Run `metacore-cli migrate --in addon.json --out addon.v3.json`.
2. Diff the output, tighten `compatibility.requires[].version` ranges.
3. Replace `Events []string` with `extension_points.events[]` and add a
   `payload_schema` for each event.
4. Move CRUD hooks from `lifecycle_hooks` into
   `contributions.subscriptions[]`.
5. Re-sign with the new ed25519 marketplace key and update `signature`.
6. Publish.
