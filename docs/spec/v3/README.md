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

## Contribution conditions

A contribution (`navigation` group/item, `actions[]`, `slots[]`,
`dashboard[]`, `routes[]`) may carry a `condition` block: an **org-level**
predicate the HOST resolves at serve time. It is not the same thing as
`visible_when` (a client-side, single-sibling *form* predicate the SDK
evaluates against the record being edited).

```json
"condition": { "addon_installed": "workshop" }
"condition": { "connector_connected": "factura_com" }
"condition": { "connector_connected": "factura_com", "field": "fiscal_uuid", "operator": "falsy" }
```

- `addon_installed` — a SOFT dependency on another addon: surface this only in
  orgs that installed it, without requiring it at install time.
- `connector_connected` — the contribution talks to a third party and is
  inoperable until that integration is **connected** for the org (credentials
  supplied, integration healthy). The key must name one of **this** manifest's
  own `connectors[]`; a foreign key can never become connected, so the
  validator rejects it. This is what stops a "Timbrar" / "Enviar por WhatsApp"
  / "Cobrar con tarjeta" button from looking operational on an org that never
  configured the PAC, the gateway or the device.
- `field`/`operator`/`value` — the record-level gate, evaluated per row by the
  client. It composes with the org-level predicates above.

### `unmet`: hide or disable

`unmet` selects what the host does when an org-level predicate fails:

| value | behaviour |
| --- | --- |
| `hide` | the contribution is not served at all |
| `disable` | the contribution is served but marked inert, carrying a machine-readable reason the SDK renders as a tooltip |

The default is **per predicate**, and it encodes a UX rule rather than a
technical one: an unmet `connector_connected` gate is *remediable* — the
operator can go connect it — so it defaults to `disable`, because a button
that silently vanishes teaches nothing while a disabled one names the fix. An
unmet `addon_installed` gate is not something the operator can act on from
that screen, so it defaults to `hide`.

A host that cannot resolve a predicate (an older host that does not know
`connector_connected`) treats it as **met**: an upgrade must never make every
button vanish. See `v3.Condition.SatisfiedBy` and `UnmetPolicy`.

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

## Additive revisions inside v3

v3 was frozen in kernel v0.13.0 and has since grown additively (all backwards
compatible — existing v3 manifests validate unchanged). The
[`manifest-v3.schema.json`](./manifest-v3.schema.json) in this folder tracks
the authoritative embedded schema; the table below summarises what each kernel
release added:

| Kernel | Field(s) added                                                                                          |
| ------ | ------------------------------------------------------------------------------------------------------- |
| v0.14.0 | `contributions.actions[]` modals: `Action.icon`, `Action.fields[]`, `Action.modal`, `Action.confirm`, `Action.confirm_message`; new `ActionField` / `FieldOption` / `FieldValidation`; top-level `frontend` block (federated UI). |
| v0.15.0 | `settings[].description`, `settings[].type: "number"`, `models[].columns[].comment`, handler `type: "compiled"`. |
| v0.16.0 | `ActionField.item_fields[]` — declarative repeatable line-items group (a `type: "array"` field whose cell columns are themselves `ActionField`s). |
| v0.17.0 | `kind: "Preset"` resolution + install (see [Kinds](#kinds)); `preset.addons[]` + `preset.defaults`. |
| v0.18.0 | wasm action triggers validate without a `backend` block (their handlers are the export surface). |
| v0.19.0 | `metadata.i18n` — marketplace catalog localizations keyed by locale (`{ "es": { name, description, features }, … }`). Distinct from the top-level `i18n` block (app string-bundle pointers); the flat `metadata.name`/`description`/`features` are the per-field fallback. |
| v0.20.0 | `metadata.countries[]` — ISO 3166-1 alpha-2 codes the addon targets (empty = global). The hub filters the catalog by the user's country. |
| v0.111.0 | `contributions.public_routes[]` — org-scoped, token-addressed public views of a record served by the host without a login (see [Public routes](#public-routes)). New `PublicRoute` type; `enabled_when` record predicates parsed by `v3.ParseRecordExpr`. |

`metadata.i18n` and `metadata.countries` slot into the `metadata` block:

```jsonc
"metadata": {
  "key": "waybill", "version": "1.2.0", "name": "Carta Porte",
  "countries": ["MX"],
  "i18n": {
    "es": { "name": "Carta Porte", "description": "Complemento SAT" },
    "en": { "name": "Waybill",     "description": "SAT complement"  }
  }
}
```

## Public routes

`contributions.public_routes[]` lets an addon publish a **login-less, org-scoped
view of a single record** addressed by a per-record secret token: a shareable
quote, an order-tracking page, a customer statement, a waiting-room screen. The
host serves every entry at

```
GET /p/<orgRef>/<addon>/<key>/<token>[.pdf|.json]
```

(`<orgRef>` is omitted when the request arrives on the org's own custom domain.)
The lookup is always scoped to the resolved organization — a token never
resolves across tenants — and the host answers `404` for an unknown token,
`410` once `expires_column` is in the past and `404` when `enabled_when` does
not hold for the record.

```jsonc
"contributions": {
  "documents": [
    { "key": "quote", "model": "Quote", "template": "templates/quote.html", "paper": "A4" }
  ],
  "public_routes": [
    {
      "key": "quote_public",              // <key> path segment, unique in the addon
      "model": "Quote",                   // own model (or one this addon extends)
      "token_column": "public_token",     // text column holding the per-record secret
      "kind": "document",                 // document | json | html
      "document": "quote",                // documents[] key (required for kind document)
      "expires_column": "valid_until",    // optional date/timestamp → 410 once past
      "enabled_when": "status != 'draft'",// optional record predicate
      "label": "quotes.public.link"       // i18n key (or literal) for the UI affordance
    },
    {
      "key": "order_tracking",
      "model": "Order",
      "token_column": "tracking_token",
      "kind": "json",
      "columns": ["folio", "status", "eta"],   // allowlist — nothing else is serialised
      "relations": ["customer"]                // refs → {value,label}; children → rows
    }
  ]
}
```

| Field            | Rule                                                                                                   |
| ---------------- | ------------------------------------------------------------------------------------------------------ |
| `key`            | Required, snake_case, unique within the addon.                                                        |
| `model`          | Required. A model the addon owns, or one it extends (`models[].extensions[].target_model`).          |
| `token_column`   | Required. A `text` / `varchar(n)` column of `model`. The host generates and rotates it (32 random bytes, base64url) and compares it in constant time. |
| `kind`           | Required. `document` streams the PDF of `document`; `json` returns only `columns` (+ `relations`); `html` serves a minimal branded page with an optional PDF button. |
| `document`       | A `contributions.documents[]` key bound to the same model. Required for `document`; optional for `html` / `json`. |
| `columns`        | Allowlist for `json` / `html`. Required for `json`; for `html` may be empty only when `document` is set. The token column is never allowed. |
| `relations`      | Allowlist of `models[].relations[].name` or ref column stems (`customer` for `customer_id`).          |
| `expires_column` | Optional `date` / `timestamp` / `timestamptz` column; once in the past the host answers `410 Gone`.    |
| `enabled_when`   | Optional predicate: comparisons `<column> <op> <literal>` (`== != < <= > >= in not_in`) joined by `&&` / `\|\|`, parentheses allowed. Parsed at validation time by `v3.ParseRecordExpr`; evaluated by the host on every request. |

Column-level checks run for models the addon owns; for extended models they are
deferred to the host at serve time (their columns live in another manifest).

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
