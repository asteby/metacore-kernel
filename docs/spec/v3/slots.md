# UI slots: `contributions.slots[]` and the `ops.host/v1` catalog

A slot is a named place in a host screen where an addon mounts a federated
React component. The manifest declares WHAT to mount and WHERE; the host owns
the mounting (error boundary, timeout, ordering). The kernel only carries the
contract and validates it. It contains no host code.

## `contributions.slots[]`

```jsonc
{
  "slot_kind": "record.panel",     // a kind of the host catalog, or one published by another addon
  "entry": "./RecordPanel",        // = expose of the federated remote (frontend.container)
  "order": 10,                     // ascending; ties keep manifest order
  "permission": "hello.read",      // the host hides the slot without it
  "props": { "title": "Hola" },    // NEW, optional: static JSON handed to the component as props
  "timeout_ms": 8000,              // NEW, optional: 1000..30000, how long the host waits for the remote
  "fallback": "hide",              // NEW, optional: "hide" (default) | "notice"
  "condition": { "addon_installed": "x" }
}
```

All new fields are optional: a manifest that does not use them validates and
behaves exactly as before.

| Field | Rule |
|---|---|
| `props` | JSON object. Static: no tokens, no code. Never put secrets here, the manifest is public to every viewer of the slot. |
| `timeout_ms` | Integer, 1000..30000 (schema error outside). Omitted: the host default. |
| `fallback` | `hide` renders nothing when the contribution fails to load or throws; `notice` renders a neutral inline notice. Either way a broken remote never takes the host shell down. |

## Catalog `ops.host/v1`

A known list, exported as `v3.HostSlotCatalogOpsV1`. Hosts implement the slots
they choose to; this list only says which names are not typos.

| `slot_kind` | Context handed to the component |
|---|---|
| `dashboard.widget` | none |
| `record.panel` | `{ model, record_id }` |
| `record.action` | `{ model, record_id }` |
| `list.toolbar` | `{ model }` |
| `settings.section` | none |
| `topbar.item` | none |
| `page.header` | `{ route }` |

Contexts other than `record.panel`, `list.toolbar` and `page.header` are the
host's to define; the kernel does not validate them.

An addon can also publish its own kinds in `extension_points.slot_kinds[]`
(`name`, optionally `props_schema`) for other addons to contribute to.

## Tolerated rules (warnings, never errors)

All three are registered in `manifest/v3/compat.go` and are warning-only in
BOTH modes (`Options.Strict` included) for their first minor, like
`webhook_action_signing`: an already-published addon is never rejected at
publish because of them. Warnings read `[rule_id] message`.

| Rule | Fires when |
|---|---|
| `slot_kind_unknown` | a `contributions.slots[].slot_kind` is not in the catalog above and not published by this manifest's own `extension_points.slot_kinds` (it may be published by another addon, which one manifest cannot see). Both `name` and `<addon_key>.<name>` match. |
| `private_frontend_integrity` | `metadata.visibility = private`, a `frontend` block exists and `frontend.integrity` is empty. |
| `private_explicit_capabilities` | `metadata.visibility = private`, any handler is `wasm` (actions, subscriptions, tools, `provides_capabilities`, `webhooks[].do` / `schedules[].do` of `wasm:<export>`) and `capabilities[]` is empty. |

Promotion path: the minor after the rules ship, move them from `warnOnlyWindow`
to plain `tolerate` (error in strict mode) and then to errors.

## Runtime helpers

The executors behind two declarative primitives are exported as host-neutral
Go packages. They never touch a database or HTTP; the host wires them into its
own transaction.

- `runtime/setfields`: `Apply(Spec, Input) (Result, error)` runs a
  `handler.type: set_fields` action (RFC-02): `requires_state` gate, then token
  resolution (`$now`, `$actor.id`, `$payload.<f>`, `$record.<c>`, `$$` escape;
  `$now` and the actor always come from the server), then the before/after
  delta. `Columns` gives the `WithProtectedWrite` allowlist.
- `runtime/ingest`: `Decode(body, IngestSpec)` and
  `Apply(ctx, IngestSpec, Batch, Store) (Result, error)` run a `Model.ingest`
  sink (RFC-04): size and batch limits, key / field allowlist (managed columns
  never writable), cursor staleness (`<=` is skipped), soft delete, per-item
  error codes (`validation`, `unknown_field`, `missing_key`,
  `constraint_violation`) and `high_water`. The host implements `Store` bound
  to the organization of the ROUTE, never of the body.
