# Module Contract v3

`apiVersion: asteby.com/v3`

The Module Contract is the single declarative source of truth for any unit of
extensibility the metacore kernel installs: foundation addons, vertical
presets, themes and connector packs. v3 is the first revision designed end to
end around three principles that v2 only paid lip service to:

1. **Manifest is the contract, code is an implementation detail.** Every
   capability the kernel grants, every event the runtime routes, every
   schema the database materialises and every UI slot the host renders is
   declared in the manifest. The kernel never introspects the addon binary
   to discover behaviour; if it is not in the manifest, it does not exist.
2. **Inter-addon coupling happens through typed events and named slots, never
   through direct imports.** An addon expressing "I want to extend the sales
   order screen" declares a contribution against a published `slot_kind`,
   not a Go/TS import path. An addon reacting to another addon declares an
   `event:subscribe` capability against a published event name, not a
   function call. This is what lets two unrelated vendors ship addons that
   compose without coordinating releases.
3. **Tenant isolation is `shared` + Postgres RLS by default.** Most addons
   should live in the shared schema with a `rls_column` so the kernel can
   inject the org filter for free. Schema-per-tenant and database-per-tenant
   are escape hatches for regulated workloads and must be justified, not
   the default.

The authoritative grammar lives in
[`manifest-v3.schema.json`](./manifest-v3.schema.json) (JSON Schema Draft
2020-12). Anything not expressible in the schema is, by definition, not part
of the contract.

## Kinds

`metadata.kind` selects the top-level shape the kernel installs:

| kind            | what it is                                                  |
| --------------- | ----------------------------------------------------------- |
| `Addon`         | A foundation module that contributes models, events, slots, RBAC and code. |
| `Preset`        | A curated bundle of addons with default settings. Installs other addons in dependency order. |
| `Theme`         | A pure visual contribution (tokens, fonts, icon overrides). No code, no models. |
| `ConnectorPack` | A set of credentials + capability templates for third-party APIs (Stripe, Mercado Pago, …). |

A manifest of `kind: "Addon"` may not use `preset.*`. A manifest of
`kind: "Preset"` must declare `preset.addons[]` and may not declare its own
`models[]`, `contributions[].models[]` or `lifecycle.upgrade[]`. The schema
enforces these mutual exclusions; the kernel rejects mismatched bundles at
install time.

## Top-level layout

```jsonc
{
  "apiVersion": "asteby.com/v3",
  "kind":       "Addon",

  "metadata":        { "key": "...", "version": "...", ... },
  "compatibility":   { "requires": [ ... ] },
  "tenancy":         { "isolation": "shared", "rls_column": "organization_id" },
  "capabilities":    [ { "kind": "db:write", "target": "addon_inventory.*" } ],
  "models":          [ { "key": "Product", ... } ],
  "contributions":   { "navigation": [...], "slots": [...], "actions": [...] },
  "extension_points":{ "events": [...], "slot_kinds": [...], "model_extensions_accepted": [...] },
  "lifecycle":       { "install": "...", "upgrade": [...], "uninstall": "..." },
  "i18n":            { "default_locale": "es-MX", "bundles": [...] },
  "rbac":            { "roles": [...], "permissions": [...] },
  "settings":        [ { "key": "...", "type": "string", ... } ],
  "billing":         { "metered_events": [...] },
  "signature":       { "algorithm": "ed25519", "key_id": "...", "value": "...", "signed_at": "..." }
}
```

Every block is documented in the JSON schema with `description`, `examples`
and `required` arrays. Use a schema-aware editor (VS Code with the JSON
extension, `jsonschema` CLI, `ajv-cli`, etc.) for first-class authoring
support.

## Compatibility model

`compatibility.requires[]` replaces the v2 trio of `kernel`, `requires` and
implicit "must match the host version". Every entry is:

```json
{ "key": "kernel", "version": ">=3.0.0 <4.0.0", "optional": false, "reason": "..." }
```

`key: "kernel"` is reserved and identifies the host kernel itself. Any other
key references another addon by `metadata.key`. `version` is a semver range
parsed by Masterminds/semver. `optional: true` lets the addon enable extra
features when a peer is present without failing the install if it is not.

## Inter-addon coupling

Two addons coordinate exclusively through `contributions` (the consumer side)
and `extension_points` (the publisher side):

- An addon publishing `extension_points.events[]` declares the event names
  other addons may subscribe to. Each entry has a `payload_schema` ref so
  consumers can validate at compile time.
- An addon publishing `extension_points.slot_kinds[]` declares the UI
  extension surfaces it owns. Consumers contribute via
  `contributions.slots[]` referencing a published `slot_kind`.
- An addon publishing `extension_points.model_extensions_accepted[]` opts
  into letting other addons attach columns to its models via
  `models[].extensions[]`.

The host kernel rejects subscriptions to undeclared events, contributions to
undeclared slot kinds and extensions of models that did not opt in.

## Tenancy

```json
"tenancy": {
  "isolation":  "shared",
  "rls_column": "organization_id"
}
```

- `shared` (default): all tenants share the addon schema; every row carries
  `rls_column` and the kernel installs a Postgres RLS policy that filters
  by the current org. This is correct for ~95% of addons.
- `schema`: the kernel creates `addon_<key>_<orgshort>` per installation and
  drops it on uninstall.
- `database`: reserved; the kernel validates the value but currently fails
  the install with `tenancy_not_implemented`.

## Capabilities

The runtime enforces capabilities by injecting a scoped `AddonContext`. The
closed set of `kind`s is:

```
db:read         db:write         http:fetch
event:emit      event:subscribe  fs:read
secrets:read    cron:register    queue:produce
queue:consume   file-storage:write
time:wallclock
```

`target` syntax depends on `kind` (a glob for db, a URL prefix for
http:fetch, an event name for event:*, a cron expression for cron:register,
etc.). The schema documents each.

## Lifecycle

```json
"lifecycle": {
  "install":   "Install",
  "uninstall": "Uninstall",
  "enable":    "Enable",
  "disable":   "Disable",
  "upgrade":   [
    { "from": ">=1.0.0 <1.3.0", "type": "wasm",     "function": "MigrateTo_1_3" },
    { "from": ">=1.3.0 <2.0.0", "type": "sql",      "function": "migrations/1_3_to_2_0.sql" }
  ]
}
```

Each `lifecycle.upgrade[]` entry describes a migration the installer runs
when upgrading an existing installation whose recorded version falls inside
`from`. `type: "wasm"` calls a function exported by the addon's wasm
backend; `type: "sql"` runs a goose-compatible SQL file from the bundle.

## Signature

```json
"signature": {
  "algorithm": "ed25519",
  "key_id":    "marketplace:asteby:2026Q1",
  "value":     "base64-ed25519-signature",
  "signed_at": "2026-05-21T15:04:05Z"
}
```

The kernel verifies the signature against the public key registered for
`key_id` before unpacking the bundle. Unsigned manifests are accepted only
in development mode (`KERNEL_ALLOW_UNSIGNED=1`).

## Migration policy

- Kernel **3.x** is **dual-read**: it accepts both v2 manifests (no
  `apiVersion`) and v3 manifests (`apiVersion: asteby.com/v3`). The
  installer transparently maps v2 fields into the v3 in-memory shape so
  downstream code only sees v3.
- Kernel **4.x** drops v2 support. Foundation addons must publish a v3
  manifest before the 4.x release train opens.

The full field-by-field mapping lives in
[`migration-v2-to-v3.md`](./migration-v2-to-v3.md).

## Examples

- [`examples/addon-example.json`](./examples/addon-example.json) — a
  foundation `Addon` (inventory) exercising models, capabilities,
  extension points, RBAC, billing and signature.
- [`examples/preset-example.json`](./examples/preset-example.json) — a
  vertical `Preset` bundling foundation addons with default settings.

## Validating a manifest

```go
import "github.com/asteby/metacore-kernel/manifest/v3"

if err := v3.Validate(raw); err != nil {
    return fmt.Errorf("invalid manifest: %w", err)
}
```

`v3.Validate` runs the JSON schema check and a small set of cross-field
invariants the schema cannot express (e.g. `kind: "Preset"` forbids
`models`). It is the same validator the kernel installer calls.
