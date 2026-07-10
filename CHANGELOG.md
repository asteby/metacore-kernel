# Changelog — metacore-kernel

All notable changes to this module are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [Unreleased]

### Added

- **manifest+dynamic: declarative action idempotency (`Action.idempotency`).** A
  v3 action may declare `idempotency: {key_field}`; the client supplies a stable
  unique value in that payload field and the kernel keys a stored response by
  `(org, model, action, key)` in the new kernel-owned `metacore_action_idempotency`
  table (lazily auto-migrated, same pattern as `metacore_addon_migrations`). A
  repeat invocation replays the first stored response WITHOUT re-dispatching the
  handler (meta `idempotent_replay: true`), so a payment capture or fiscal stamp
  survives a client retry without double-execution. Only SUCCESSFUL results are
  cached — a declined/failed action stays retryable. The guarantee is
  persisted-replay dedup, not a distributed lock (documented). New field on
  `manifest/v3.Action` + `manifest.ActionDef` with dual validation (v3 lenient +
  strict) and JSON-schema coverage; wired into `dynamic.Service.ExecAction`.

- **wasm: `data_batch` host import (ABI v1.7).** The atomic multi-row sibling of
  `data_mutate`: a list of create/update/delete mutations across one or more
  models, executed under ONE org-scoped transaction, each followed (post-commit)
  by its own `*dynamic.CanonicalEvent`. It is the primitive that lets a wasm
  handler perform "sale decrements stock + writes the ledger + updates the
  weighted cost" atomically — either every row lands or none does. Gated by
  `db:write <logical table>` for every table the batch touches; tenant scope
  comes from the invocation context, never from the guest. Limits: 64 KiB
  request (the frozen ABI ceiling — many small rows, not a few huge ones), 100
  mutations, 10 s deadline, 8 MiB response. A per-row failure rolls back the
  whole batch and publishes no events; the error names the offending
  `mutations[i]` index. The per-row engine (`applyMutation`) is now shared with
  `data_mutate`. Implementation `runtime/wasm/databatch.go`; contract
  documented in `docs/wasm-abi.md § 16`.

## [0.63.0] - 2026-07-01

### Added

- **dynamic: column facets endpoint (`GET /:model/facets`).** A new read path
  returns the distinct values of a column together with their row counts,
  org/branch scoped, so a text-column filter can offer the values that actually
  exist in the table (e.g. the real `repo` / `assignee` values in the
  `github_issues` addon) instead of only a free-text "contains…" match. New
  `Service.Facets(ctx, user, FacetsQuery) ([]FacetBucket, error)` runs
  `SELECT <col> AS value, COUNT(*) AS count FROM <table> WHERE <org scope> AND
  <col> IS NOT NULL [AND <col> <> ''] [AND <col> ILIKE <q>] GROUP BY <col>
  ORDER BY count DESC, value ASC LIMIT <n>`. `FacetsQuery{Model,Field,Q,Limit}`
  and `FacetBucket{Value,Label,Count}` (json `value`/`label`/`count`). The
  column must exist on the model and pass `safeColumn` (`ErrOptionsFieldNotFound`
  / `ErrInvalidInput` otherwise, mapped to 404/400 like options); the empty-string
  guard is applied only for text columns; the `q` filter reuses the configured
  `SearchMatchClause` (unaccent/ILIKE on Postgres) with the same `%`/`_` escaping
  as `Service.Options`; `Limit` defaults to `DefaultOptionsLimit` (50) and clamps
  to `MaxOptionsLimit` (200). Tenant scoping fires only when the model carries an
  `organization_id` column (`hasOrgColumn`). The handler emits the standard
  `{success, data:[…], meta:{count,field}}` envelope. Additive — hosts that do
  not mount the route are unaffected.

## [0.62.0] - 2026-06-26

### Added

- **manifest/v3: stage machine on models (`stage_field`, `stages[]`,
  `transitions[]`, `on_transition[]`).** A model can now declare a Bitrix-style
  pipeline: `stage_field` names the lifecycle column, `stages[]` enumerates its
  ordered stages (`{key,label,color,order,is_final}`), `transitions[]`
  whitelists the allowed `{from,to}` moves, and `on_transition[]` declares hooks
  (`{from,to,do,required}`, `*` = wildcard) the kernel fires on a valid move.
  New v3 types `Stage`, `Transition`, `TransitionHook` on `v3.Model`; mirrored
  on the host `manifest.ModelDefinition` as `StageDef`/`TransitionDef`/
  `TransitionHookDef` and projected by `FromV3`. Both the embedded
  `manifest/v3/schema/manifest-v3.schema.json` and the published
  `docs/spec/v3/manifest-v3.schema.json` gained the `Stage`/`Transition`/
  `TransitionHook` definitions so the hub's strict publish-gate accepts the
  block. Additive — a model without `stages` keeps the legacy unrestricted
  behaviour.

- **manifest/v3: kanban view-type on nav (`view_type`, `group_by`).** A
  `NavItem` can declare `view_type: "kanban"` + `group_by` so the SDK renders a
  board grouped by a column (default stays `table`). The same model can have a
  table nav and a kanban nav. Mapped through `FromV3` onto `manifest.NavItem`
  for the host to project into served `TableMetadata`.

- **dynamic: stage-transition enforcement + hooks in `Service.Update`.** When a
  host wires the new `Config.StageMachineResolver`, an `Update` that changes a
  model's `stage_field` is validated against the declared `transitions[]` BEFORE
  any write — a non-whitelisted move is rejected with the new
  `ErrInvalidTransition` (HTTP 422 `invalid_transition`). After a valid move is
  persisted and before the canonical event, the matching `on_transition` hooks
  fire: each hook's `do` (`wasm:<export>` | `webhook:<key>` | `compiled:<fn>`)
  is split on its prefix and routed through the existing `ActionDispatchers`
  with a `{before, after, actor, org}` payload. A failing hook is logged and
  skipped unless `required: true`, in which case the save + hooks (run together
  in a transaction) roll back. `stages` empty = no restriction (full
  back-compat).

- **dynamic: `status` display derived from `stages[]`.** `DeriveTableColumns`
  now sets the `stage_field` column's `CellStyle` to `status` and materialises
  its options (`value/label/color`, ordered by `Stage.Order`) straight from
  `stages[]`, so a board / status badge renders without a separate `options`
  declaration. An explicit column `display`/`options` still wins.

- **Validation (dual surface):** `v3.Validate` (lenient) and the strict
  `manifest.Validate` both enforce the stage-machine cross-field rules
  (`stage_field` names a real column; unique stage keys; transitions/hooks
  reference declared stages or `*`; `do` carries a known `wasm|webhook|compiled`
  prefix) and the kanban rule (`view_type:kanban ⇒ group_by`), so a manifest
  fails identically on both the publish and install paths.

- **manifest/v3: addon-level pipeline-runtime primitives (`connectors[]`,
  `schedules[]`, `webhooks[]`).** An addon can now declare, alongside its models:
  `connectors[]` (`{key,label,auth,credentials[]}`) — third-party credential
  providers the host stores per org (encrypted); `schedules[]` (`{key,every,do}`)
  — declarative cron jobs; and `webhooks[]`
  (`{key,path,verify,secret_ref,do}`) — inbound webhook routes. New v3 types
  `Connector`/`Schedule`/`InboundWebhook` (credentials reuse `Setting`, which
  gained an optional `validation` field) projected by `FromV3` onto host
  `ConnectorDef`/`ScheduleDef`/`InboundWebhookDef` (a `secret`-typed credential
  maps to `Secret:true`). Both JSON schemas gained the `Connector`/`Schedule`/
  `InboundWebhook` definitions. Dual validation (`v3.Validate` + strict
  `manifest.Validate`) enforces: unique connector keys; a schedule's `every`
  parses as a positive Go duration and its `do` carries a known dispatch prefix;
  a webhook's `do` carries a known prefix and, when it declares a `verify`, its
  `secret_ref` resolves (`<connector>.<credential>`) to a declared connector
  credential. Additive — an addon with none of these blocks is unaffected.

- **connectors/ (new): credential-resolution runtime.** `connectors.Store` is a
  host-implemented per-org credential backend (ops owns the encryption);
  `connectors.Resolver` resolves a connector key → credential map (`Get`) and a
  `secret_ref` → a single value (`Secret`). A nil Resolver/Store is the
  feature-off state (every lookup returns `ErrNoStore`).

- **runtime/schedule/ (new): declarative cron scheduler.** `schedule.Scheduler`
  runs one ticker per `(orgID, scheduleKey)` and fires the schedule's `do`
  through a prefix-keyed `Dispatcher` set (the same `wasm|webhook|compiled`
  mechanism as the stage-machine hooks), with a `{org, schedule}` payload. API:
  `New`, `Register`, `Unregister`, `Start`, `Stop`. Idempotent on boot
  (re-registering a key replaces its ticker, never duplicates). Time is injected
  via the `Clock` interface (`SystemClock` in prod) so fires are testable
  without sleeping.

- **runtime/webhookin/ (new): inbound-webhook receiver.** `webhookin.Receiver`
  registers the routes from `webhooks[]` and exposes
  `Dispatch(ctx, orgID, path, headers, body)` for the host's HTTP layer to call.
  It verifies the signature (`hmac-sha256`: HMAC of the raw body with the secret
  resolved from `secret_ref` via a `SecretResolver` — `connectors.Resolver`
  satisfies it; accepts a `sha256=`-prefixed or bare-hex signature in
  `X-Hub-Signature-256` / `X-Signature-256` / `X-Signature`) and routes the body
  to `do`. Returns typed errors (`ErrRouteNotFound`, `ErrSignatureInvalid`,
  `ErrSignatureMissing`, `ErrUnsupportedVerify`, …) for the host to map to HTTP
  status. An empty `verify` skips verification.

- **runtime/wasm: `connector_get` host import + `connector:read` capability.**
  Guests resolve one of their declared connector's credentials for the
  invocation's org via `connector_get(keyPtr,keyLen) -> i64` (JSON object
  envelope), gated by the new `connector:read <key>` capability
  (`security.Capabilities.CanReadConnector`) and wired through
  `Host.WithConnectors(*connectors.Resolver)`. Unconfigured resolver →
  `connector_unavailable` envelope (feature-off).

- **runtime/wasm: `http_request` host import (headers).** New
  `http_request(urlPtr,urlLen, methodPtr,methodLen, headersPtr,headersLen,
  bodyPtr,bodyLen) -> i64` sends outbound HTTP with caller-supplied request
  headers (JSON object, e.g. `{"Authorization":"token …"}`), enabling
  authenticated third-party calls. Same `http:fetch` capability, SSRF guard, 30s
  timeout and 8 MiB response cap as `http_fetch`, which is **unchanged** (it now
  delegates to the shared path with empty headers) — existing guests keep
  working. ABI bumped to 1.6 (purely additive).

## [0.59.0] - 2026-06-13

### Added

- **manifest/v3: `contributions.dashboard[]` — declarative + federated dashboard
  widgets.** Addons can now contribute tiles to the host's modular dashboard
  without per-app code. New types `DashboardWidget`, `WidgetQuery` and
  `WidgetCompare` on `v3.Contributions.Dashboard`. Two flavours coexist in one
  grid: DECLARATIVE kinds (`stat|bar|line|area|pie|donut|list|progress`) carry a
  `query` the host aggregates; the FEDERATED `custom` kind references a frontend
  `expose`. `v3.Validate` enforces the §1 cross-field rules (key/title/kind
  required; kind enum; `kind!=custom ⇒ query+query.model`; `aggregate!=count ⇒
  field`; `bar/pie/donut/list ⇒ group_by`; `line/area ⇒ date_field+interval`;
  `custom ⇒ expose+frontend`; size/format enums; unique keys; compare needs
  date_field+range). The JSON schema (both the embedded
  `manifest/v3/schema/manifest-v3.schema.json` and the published
  `docs/spec/v3/manifest-v3.schema.json`) gained the matching `dashboard`
  definitions so the hub's strict jsonschema publish-gate accepts the block.
  Dashboard is pure host/SDK UI metadata — `FromV3` does not project it into the
  legacy `manifest.Manifest`, so the legacy strict validator is unaffected.

- **query: host-side aggregation engine for the dashboard.** New
  `query.Aggregate(ctx, db, AggregateSpec) (AggregateResult, error)`, always
  org-scoped (`organization_id = ?`) and soft-delete aware
  (`deleted_at IS NULL`). Supports `count` (`COUNT(*)`) and `sum/avg/min/max`
  over a field; scalar, `group_by` (ordered by value, clamped to 1..24) and
  `date_field`+`interval` time series (`date_trunc`, dialect-aware, empty
  buckets back-filled to 0 inside the resolved `TimeRange`). The `where` map
  reuses the list builder's filter operators (`eq/neq/gt/gte/lt/lte/contains`).
  An optional `LabelResolver` turns foreign-key bucket keys into readable
  labels. Exported helpers: `AggregateBucket`, `AggregateResult`, `TimeRange`
  (+ range tokens), `Interval`, `PreviousRange` (for `compare`). No CGO — the
  engine lives in `query`, not `runtime/wasm`.

## [0.58.4] - 2026-06-12

### Fixed

- **query: delegated lists still came back in heap order.** v0.58.2 added a
  newest-first default sort, but it only fired when `created_at` was in the
  column whitelist — and the whitelist is built from the manifest, which never
  declares the framework columns `CreateDynamicTable` stamps. So `created_at`
  was absent, `defaultSort` found no timestamp, and lists (e.g. sales orders)
  rendered unsorted. `buildAllowed` now always whitelists `id`/`created_at`/
  `updated_at` — the default sort fires and users can sort by them too.

## [0.58.3] - 2026-06-12

### Fixed

- **dispatch + runtime/wasm: subscription side effects lost their actor.**
  The dispatcher detaches deliveries onto a background context (correct — the
  source request may die first), but that dropped the originating event's
  identity: every canonical event a wasm handler emitted via `data_mutate`
  surfaced as an anonymous actor in the audit trail. The dispatcher now
  re-attaches the event's `actor_id` + `correlation_id` to the delivery
  context (`dynamic.WithActorID`, new), and `data_mutate` stamps
  `CanonicalEvent.ActorID` from it.

## [0.58.2] - 2026-06-12

### Fixed

- **query: delegated lists rendered in heap order.** `applySort` was a no-op
  when `SortBy` was empty or unknown, trusting "the caller's default ordering"
  — which no caller applied. Lists served through `dynamic.Service.List`
  (every delegated addon model) now default to newest-first on the first
  timestamp column the model has (`created_at` → `occurred_at` →
  `updated_at`); models with none stay unordered as before.

## [0.58.1] - 2026-06-12

### Fixed

- **runtime/wasm: WASI reactors never initialized.** Guest modules built with
  `GOOS=wasip1 -buildmode=c-shared` export `_initialize` (reactor model), but
  module instantiation used wazero's default start list (`_start` only) — the
  Go runtime init never ran and the first `alloc` host call crashed with
  "out of bounds memory access". `WithStartFunctions("_initialize", "_start")`
  now invokes whichever entry point the module exports.

## [Unreleased]

### Added

- **feat(runtime/wasm): `metacore_host.data_query` — org-scoped logical-table
  read, the read sibling of `data_mutate` (ABI v1.5, § 15 of
  docs/wasm-abi.md).** The guest sends `{table, where?, limit?}` and gets
  `{success, data: {rows}, meta}` back: ONE equality-filtered SELECT against
  a LOGICAL table resolved through the SAME `Host.WithTableResolver` hook
  `data_mutate` writes through — NOT the addon-schema search_path `db_query`
  scopes to, whose shadow schemas exist but hold no live rows in embedding
  hosts like ops (a guest reading bare `stock` via db_query would see an
  empty table; data_query keeps the addon portable without hardcoding
  `public.*`). The host injects `organization_id = ?` (from the invocation
  context, never the guest) as the first predicate of every query, and
  appends `deleted_at IS NULL` when the table is soft-deletable — detected
  via a zero-row probe (`SELECT * FROM <tbl> LIMIT 0`) whose result-set
  metadata yields the column set (deterministic, no 42703 error-sniffing).
  `where` values are scalars only (string/number/bool/null; null compiles to
  `IS NULL`); guest filters on `organization_id`/`deleted_at` are rejected.
  `limit` defaults to 50, clamps at 200. Gated by `db:read <logical table>`
  through the same `security.Enforcer` (a `db:write` grant implies read);
  limits mirror `data_mutate` (request 64 KiB, deadline 5 s, response
  8 MiB); same v1 envelope and error codes. Pure read: no transaction, no
  events.

- **feat(runtime/wasm): `metacore_host.data_mutate` — structured org-scoped
  row mutation + post-commit canonical event (ABI v1.4, § 14 of
  docs/wasm-abi.md).** The guest sends `{op, table, model, id?, data?, inc?,
  returning?}` (op: create/update/delete; `inc` compiles to atomic
  `col = col + delta` SETs); the host owns tenant scope (`organization_id`
  ALWAYS from the invocation context, never the guest), id/timestamp stamps,
  soft-delete when the row carries `deleted_at`, and the transaction. After
  COMMIT the host publishes the matching `*dynamic.CanonicalEvent`
  (`<addonKey>.<Model>.<created|updated|deleted>`, with `CorrelationID` from
  the context) on the events.Bus, so guest mutations feed the same event
  stream as the host's dynamic CRUD. The physical table comes from the new
  embedder hook `Host.WithTableResolver(func(table string) string)` (identity
  by default) — NOT from the addon-schema search_path `db_exec` uses, because
  writes must land in the live host data (in ops: `public.*`). Gated by
  `db:write <logical table>` through the same `security.Enforcer` as
  `db_exec`; limits mirror `db_exec` (request 64 KiB, deadline 5 s, response
  8 MiB); envelope `{success, data: {id, before, after}, meta}` v1. Unlike
  `db_exec`, the import always opens its own short-lived transaction (never
  the action handler's tx) so the event can only ever describe committed
  state.

- **feat(manifest/v3): `options_source` — declarative DYNAMIC options on a
  column.** A column names a host-registered provider key (e.g.
  `"registered_models"`, `"installed_addons"`) instead of hardcoding an
  `options` list; the HOST resolves the provider at metadata-serve time and
  materialises the localized value/label list onto the served `options`. Open
  enum (pattern `^[a-z][a-z0-9_]*$`): the kernel only carries the key — no
  providers in the kernel. Full passthrough: v3 `Column.options_source` →
  `manifest.ColumnDef.OptionsSource` (FromV3) → served
  `modelbase.ColumnDef.OptionsSource` / `modelbase.FieldDef.OptionsSource`
  (DeriveTableColumns / DeriveFormFields; the form field derives as a
  `select`). Both validation planes agree: the strict v3 jsonschema gates the
  pattern and the legacy `Manifest.Validate` enforces the same alphabet.

---

## [0.51.0] - 2026-06-09

### Added

- **feat(dynamic): table footer totals — a declarative per-column SUM over the
  FILTERED set.** A column opts in via its manifest `display_config.aggregate:
  "sum"` (mapped by the kernel to `styleConfig.aggregate = "sum"` at runtime —
  no contract change, `display_config` is free-form). New read route
  `GET /dynamic/:model/aggregate?<same query as the list>` returns
  `{success, data: {<col>: <number>}}` — the SUM of each aggregate-flagged column
  over the same `f_*`, search and org/branch scope as the list, with NO sort and
  NO pagination (so the footer matches the filtered set, not the visible page).
  - `Builder.ApplyForAggregate` mirrors `ApplyForCount` (relation filters +
    filters + search) and adds the aggregation SELECT + GROUP BY, deliberately
    omitting sort/pagination/preloads (a non-grouped aggregate ordered by a
    column 42803s, same hazard as count).
  - `dynamic.Service.Aggregate(ctx, model, user, params, columns)` builds one
    `SUM(col) AS col` per requested column, reusing the SAME scope and table
    resolution as `List`. Columns flow through the builder's existing whitelist
    (`applyAggregations`: allowed-set + `isSafeIdent`), so a raw column name can
    never be interpolated.
  - The `aggregate` handler reads the model's `TableMetadata` and selects the
    columns whose `StyleConfig["aggregate"]` is a non-empty string.

## [0.50.1] - 2026-06-09

### Fixed

- **fix(query): `Count` no longer applies the `ORDER BY`.** `Builder.Apply`
  chains `applySort`, and `dynamic.Service.List` reused it to build the COUNT(*)
  query — so a sorted list emitted `SELECT count(*) ... ORDER BY <col>`, which
  Postgres rejects for a non-grouped column ("must appear in the GROUP BY
  clause", SQLSTATE 42803), 500ing the whole list. New `Builder.ApplyForCount`
  applies only the WHERE-shaping clauses (relation filters, column filters,
  search); `List` uses it for the count. Sorted/filtered declarative lists now
  count correctly.

## [0.50.0] - 2026-06-08

### Added

- **feat(compute): declarative compute engine — Tier-1 ROLLUP and Tier-2 FORMULA
  fields, recomputed automatically by a generic, domain-agnostic kernel engine
  with ZERO addon code (Salesforce Roll-Up Summary + Odoo stored-computed
  parity).** Two new OPTIONAL manifest blocks (every existing manifest keeps
  validating):

  - **Tier-1 rollup** — declared on a PARENT model's relation:
    `models[].relations[].rollups[] = [{ "target": "<parent col>", "fn":
    "sum|count|avg|min|max", "from": "<child col>" }]`, or with an arithmetic
    expression aggregated over the children:
    `{ "target": "total", "fn": "sum", "expr": "subtotal + tax_amount" }`. On
    every CHILD create/update/delete the engine recomputes every rollup target
    on the affected parent via ONE `UPDATE … SET col = COALESCE((SELECT <FN>(…)
    FROM child WHERE fk = ?), 0) … WHERE id = ? AND organization_id = ?`. The
    child subquery is scoped ONLY by the FK (child tables are not org-scoped);
    the parent UPDATE is org-scoped. Delete uses a `BeforeDelete` recompute with
    an `AND id <> ?` exclusion (no `AfterDelete` state handoff). `count` →
    `COUNT(*)`; empty child set zeroes the parent via `COALESCE(…,0)`.

  - **Tier-2 formula** — declared on the model:
    `models[].formulas[] = [{ "target": "<col>", "expr": "quantity *
    unit_price - discount" }]`. Evaluated on that model's own `BeforeCreate` /
    `BeforeUpdate` against the incoming input (merged with the existing row on
    update) and written into the input map BEFORE the DB write, so a line item
    self-computes and the parent rollup sums correct values within one write.

  - **Tier-3 (reserved)** — wasm-backed computed fields; contract reserved, not
    implemented here.

  Engine lives in `dynamic/compute.go` (+ `dynamic.RegisterComputeHooks`, wired
  in `installer.go` right after `RegisterManifestHooks`), with a small, safe
  numeric expression engine in the new leaf package `manifest/computeexpr`
  (recursive-descent parser shared by the runtime and both validators —
  `Eval`/`RenderSQL`/`Validate`). SECURITY: rollup/formula `expr` is parsed with
  a strict allowlist (identifiers resolving to real child/own columns, decimal
  numbers, `+ - * /` and parentheses only — no quotes, semicolons, comments,
  function calls or SQL keywords); every identifier is double-quoted when SQL is
  built, so the raw-SQL Tier-1 `expr` path cannot inject. Types
  (`Rollup`/`Formula`) land on BOTH the v3 (`Rollup`, `Formula`) and legacy
  (`manifest.Rollup`, `manifest.Formula`) contracts and are carried through
  `FromV3` (`mapRollups`, `mapModelFormulas`). Validation added to ALL THREE
  surfaces — the v3 JSON schema (`manifest-v3.schema.json`: `Rollup`/`Formula`
  defs + `rollups`/`formulas` arrays, `fn` enum), v3 `Validate` (cross-field:
  rollup target on the parent, `from` on the child, formula target/identifiers
  on the model), and the legacy/install `validateStrict` (same checks, the one
  the hub enforces) — so a manifest cannot pass v3 yet 400 on install.

- **feat(v3): column `ref` (FK → dynamic_select) and `options` (static select)
  so the NATIVE create/edit form of any addon renders a relation picker / select
  with no custom action.** The v3 `Column` gains two OPTIONAL fields (every
  existing manifest keeps validating):

  - `ref` — names the FK target model/table the column points at. The host
    projects it onto `modelbase.ColumnDef.Ref` / `modelbase.FieldDef.Ref` so the
    SDK renders a searchable `dynamic_select` resolving against
    `/api/options/:ref`. It is the explicit, declarative twin of the host's
    belongs_to auto-derivation (no relation or name heuristic required).
  - `options` — a static value/label choice list (reusing `FieldOption`, so each
    option carries the optional `icon`/`color`/`image` visuals from the prior
    release). Projected onto `modelbase.ColumnDef.Options` /
    `modelbase.FieldDef.Options` so the SDK renders a plain `select`.

  Projection path: `FromV3` carries both onto `manifest.ColumnDef` (`Ref`
  already existed; `Options []Option` is new), and `dynamic.DeriveTableColumns`
  + `dynamic.DeriveFormFields` forward them to the served metadata —
  `DeriveFormFields` additionally sets the field `type` to `dynamic_select`
  (when `ref` is set) or `select` (when `options` is set) so the native modal
  picks the right widget. Both are pure UI metadata; the DDL/install plane
  ignores them. Schema (`manifest-v3.schema.json`, `additionalProperties:false`)
  and the docs spec mirror gain the two Column props.

- **feat(v3): generic select/option visuals, dynamic_select remote-label
  mapping, image/upload form widget, and column display i18n round-trip
  (audit S5 + S6).**
  Extends the v3 contract with GENERIC (non-niche) visual + localization hints,
  all OPTIONAL so every existing manifest keeps validating:

  - **S5.1 — static option visuals.** `FieldOption` gains `icon` / `color` /
    `image` so a static `select` option list can render a coloured status dot, a
    brand icon or a thumbnail. `FromV3` forwards them onto `manifest.Option` and
    they land on the served `modelbase.OptionDef` (which gains `image`;
    `color`/`icon` already existed).
  - **S5.2 — dynamic_select remote-label mapping.** `ActionField` gains
    `label_image` / `label_icon` / `label_color`, each naming a column on the
    REMOTE model a relation picker resolves against, so a product thumbnail /
    brand icon / status colour renders beside each option without hardcoding
    per-option visuals. Forwarded verbatim onto `manifest.FieldDef` and
    `modelbase.FieldDef`.
  - **S5.3 — image / upload form widget.** The v3 `Column` gains `widget` (the
    DDL-agnostic FORM input plane, distinct from the read-only `display` cell
    plane): a `text` column can declare `widget: "image"` / `"upload"` so a
    photo/logo/attachment renders a file picker. `FromV3` carries it onto
    `manifest.ColumnDef.Widget`, which `dynamic.DeriveFormFields` already honours.
  - **S6 — column display i18n.** The v3 `Column.label` (documented as an i18n
    KEY) now survives the v3 → host conversion: `FromV3` carries it onto a new
    `manifest.ColumnDef.Label`, and `dynamic.DeriveTableColumns` /
    `DeriveFormFields` prefer the declared label over the humanized column name.
    When the label is an i18n key (host-configured prefix, e.g. `models.`) the
    host's `metadata.NewLocalizedTableTransformer` resolves it to the browsed
    locale at serve time instead of the SDK rendering the raw key. The two-stage
    DERIVE→LOCALIZE contract is documented on `DeriveTableColumns`. Previously
    the declared label was dropped entirely.

  Both the embedded JSON schema and the `docs/spec/v3` copy permit the new
  properties (the schema is `additionalProperties:false` + `DisallowUnknownFields`,
  so without this a manifest using them was rejected at publish/install).

- **feat(installer): bootstrap validation of `handler.type=compiled` symbols
  (audit S7).** Addons can declare `handler: { type: "compiled", function:
  "<symbol>" }` on actions / tools / subscriptions, but the Go implementation
  lives in the HOST (e.g. ops). The installer now extracts every declared
  compiled handler from the bundle's v3 manifest and, when the host wires a
  `CompiledHandlerRegistry` (via `Installer.WithCompiledHandlers`), HARD-FAILS
  the install with a clear, per-handler error if a symbol is unregistered —
  instead of installing "successfully" and 403-ing on first dispatch. When no
  registry is wired (the default) it SOFT-WARNS one log line per declared
  compiled handler, so hosts that have not adopted the registry are not blocked.
  The gate runs before any DB/schema state is created, symmetric in Install and
  Upgrade, and is zero-cost for wasm/webhook/declarative addons.

- **feat(v3): column display hints (`display` / `display_config` / `tooltip` /
  `description`) + auto-detected cell styles.**
  The v3 `Column` contract gains four OPTIONAL display fields so declarative
  addon tables can express how a column RENDERS (a clickable URL, the creator's
  avatar, a currency, a status badge) instead of always falling back to plain
  text. They are pure UI metadata — the DDL/installer plane ignores them exactly
  like `label`/`comment`.

  - `manifest/v3` `Column` adds `display`, `display_config`, `tooltip`,
    `description`. The embedded JSON schema (and the `docs/spec` copy) now permit
    them on a column (the schema is `additionalProperties:false` +
    `DisallowUnknownFields`, so without this a manifest using `display` was
    rejected at publish/install).
  - `FromV3` projects them onto the legacy `manifest.ColumnDef` carrier
    (`display→CellStyle`, `display_config→StyleConfig`, `tooltip→Tooltip`,
    `description→Description`, and `display_config.base_path→BasePath`), which
    `dynamic.DeriveTableColumns` copies onto the served `modelbase.ColumnDef`.
  - `dynamic.DeriveTableColumns` AUTO-DETECTS a cell style by column name/type
    when none is declared: `*_url`/`url`/`website`→`url`, `email`→`email`,
    `phone`→`phone`, `*_by`/`created_by`/`owner`→`creator`, money names on a
    numeric type→`currency`, `status`/`estado`→`status`, `color`→`color`,
    `*_at`/date type→`date`, boolean→`boolean`, image names→`image`. An
    author-declared cell style always wins; the plain `text` fallback is kept.

## [0.24.0] - 2026-05-28

### Fixed

- **fix(bundle): hydrate `Manifest.I18n` from `locales/*.json` in v3 bundles.**
  v3 manifests carry only file PATHS in `i18n.bundles[*]`; `FromV3.mapI18n`
  emitted empty inner maps so `Manifest.I18n[locale]` was always `{}`. The
  hub's `/v1/addons/{key}/i18n/{lang}.json` returned that empty payload, and
  every consumer (sidebars, dashboards, action labels) rendered the raw i18n
  keys — `accounting.nav.group` instead of `Contabilidad`.

  `bundle.Read` now post-processes the archive: for each declared
  `i18n.bundles[*]` entry it looks up the matching `locales/<file>.json`
  among the tar entries, parses it (nested objects supported), and flattens
  every string leaf into `Manifest.I18n[<locale>]` as dotted keys. A
  base-language alias is also written (e.g. `es-MX` → `es`) so hosts
  normalizing browser tags to the bare language code still resolve. The new
  `Bundle.Locales` map exposes the raw file bytes for callers that want them.

  Adds `bundle.Write` symmetry: locales round-trip through Read → Write so
  in-memory bundle builders (CLI / tests) preserve i18n payloads.

  Hub republish is required for the addons whose bundles already shipped —
  the locales files are in the tarball, this change just teaches the parser
  to inline them on Read.

### Added

- **feat(marketplace): dep-block + cascade on uninstall.** The uninstall
  handler now refuses to remove an addon some OTHER active installation in
  the same org declares under `compatibility.requires[]`; without the gate
  the dependent silently broke. The 409 response carries a
  `data.dependents[]` list so the UI can render "primero desinstalá X".

  Two escape hatches let the operator push through:

    - `force: true`   — skip the dep-block. Admin override; the dependent
      will be left in a broken state.
    - `cascade: true` — walk the reverse dep graph and uninstall every
      dependent leaf-first before the requested addon. Returns the full
      uninstall list under `data.uninstalled` plus `data.primary` pointing
      at the requested addon.

  Implementation notes:

    - New `Installation.Requires` column (`AddonKeyList`, JSON-serialised)
      captures the addon-level dep keys at install time from the bundle's
      preserved v3 `RawManifest`. Existing rows that pre-date the column
      stay empty (legacy installs are never the source of a 409 — only the
      target). The reserved `"kernel"` requirement is filtered: it is a
      runtime range, not an addon dep.
    - Dep lookup is a single query (`organization_id`, `status='installed'`)
      followed by an in-Go scan to dodge JSON `LIKE` substring collisions
      (e.g. `customers` vs `customers_pro`).
    - Cascade is BFS over the reverse-dep tree, then reversed for
      leaf-first execution. Cycles are tolerated (visited set); planned
      victims that vanished mid-flight are skipped silently.
    - Five new tests cover: dep-block fires + lists dependents, no-deps
      case still passes, force-override bypasses, cascade uninstalls
      leaf-first with the primary last, and cascade on a leaf still
      uninstalls just the requested addon.

## [0.23.0] - 2026-05-28

### Fixed

- **fix(marketplace): inject central counter-signature from response header.**
  v0.22.0 landed the trust anchor end (hub counter-signs, kernel auto-fetches
  the pubkey) but the marketplace install/upgrade HTTP path threw away the
  signature: `fetchBundle` parsed the tarball through `bundle.Read` and
  ignored the `X-Asteby-Marketplace-Signature` response header. The bundle
  reached `installer.Install` with `Manifest.Signature == nil`, the security
  gate fired `ErrUnsignedBundle`, and every install via the iframe embed
  failed with **"installer: bundle signature rejected: security: bundle has
  no signature"** even though the hub had signed correctly.

  Now `fetchBundle` calls `injectCentralSignature` to hoist
  `X-Asteby-Marketplace-Signature` (+ optional `X-Bundle-Checksum`) into the
  in-memory `manifest.Signature` before returning. Publisher-embedded
  signatures still take precedence — the multi-key trust model in
  `security.VerifyBundle` accepts either.

  Two new public constants under `installer.HeaderMarketplaceSignature` /
  `HeaderBundleChecksum` lock the header names against hub drift; a unit
  test (`TestInjectCentralSignature_HeaderConstantsMatchHub`) trips before
  a real install fails in prod.

## [0.22.0] - 2026-05-28

### Added

- **feat(installer): central marketplace trust anchor (Let's Encrypt style).**
  The installer's `New()` constructor now resolves trusted Ed25519 keys in
  this priority order:

    1. `MARKETPLACE_PUBKEY` / `MARKETPLACE_PUBKEYS` env (operator-pinned).
    2. `GET {MARKETPLACE_URL}/v1/marketplace/pubkey` (best-effort fetch
       at boot — fail-closed if both are empty and `ALLOW_UNSIGNED_BUNDLES`
       isn't set).

  `MARKETPLACE_URL` defaults to `https://hub.asteby.com` so SaaS hosts
  (ops, link, future apps) get the central anchor with zero env config.
  Customer-on-VPS deployments inherit the same behaviour when they have
  network reachability to a hub; air-gapped or pinned deployments set
  `MARKETPLACE_PUBKEY` explicitly to bypass the fetch.

  The previous model required listing every developer pubkey in env
  (impractical: the hub has many publishing developers, and the set
  changes on every new registration). Trusting ONE central
  hub-counter-signed key replaces that.

  Hosts that rotate keys at runtime call `Installer.AppendTrustedPubKey(hex)`
  on the live installer — the kernel's "verify under ANY trusted key"
  semantics keep in-flight bundles signed by the previous key valid until
  the host drops it on the next deploy. New exported helper
  `installer.FetchCentralPubKey(ctx, baseURL)` drives the same fetch from
  application code (refresh on a rotation event without a restart).

  The hub-side counter-sign and `/v1/marketplace/pubkey` endpoint ship in
  hub's matching PR.

## [0.21.0] - 2026-05-28

### Added

- **feat(installer): cover the fullstack install pipeline (#93).** The
  installer now drives the four artifact families a v3 bundle ships, not
  just SQL + lifecycle hooks. Closes the gaps that historically forced
  each host (ops, link) to reimplement post-install glue.

  - **i18n catalog persisted kernel-side.** New `metacore_addon_i18n`
    table (migration 0007) + `i18n.RegisterAddonI18n` / `GetAddonI18n` /
    `UnregisterAddonI18n` collapse what ops had as a workaround in
    `services/addonbundle/loader.go` (lines ~189-212) into the kernel.
    Both source shapes (`manifest.I18n[lang][key]` and v0.19.0's
    `metadata.i18n[lang]`) project into the persisted table — `metadata.i18n`
    unfolds to `catalog.name`/`catalog.description`/`catalog.features.<i>`
    dotted keys so the existing flat-key translator picks them up
    transparently. Install / Upgrade upsert (purge-then-upsert so renamed
    keys disappear in lockstep with the new version); Uninstall drops the
    rows. Every host (ops, link, future apps) inherits the registration
    path for free.

  - **WASM backend runtime auto-materialised.** The bundle reader has
    populated `Bundle.Backend` since the early iterations and v0.18.0
    validates wasm action triggers without a backend block, but nothing
    connected those to the installer — `host.LoadWASMFromBundle` had to
    be called by hand. New `installer.BackendRuntimeLoader` interface
    (kernel layer) with `NoopBackendRuntimeLoader` default; Install /
    Upgrade fire `LoadFromBundle` whenever `manifest.Backend.Runtime`
    selects an in-process runtime (predicate excludes nil + "webhook").
    `host.EnableWASM` now wires the existing `host.LoadWASMFromBundle`
    helper into `Installer.BackendRuntime` automatically — apps that opt
    into WASM get the integration with zero extra boot wiring. The
    declared-exports cross-check from v0.18.0 is honoured by the existing
    `wasm.Host.Load` path; the installer just hands the bundle across.

  - **Preset orchestration: topo-sort on `requires`.** `InstallPreset`
    already drove an idempotent per-addon install through a host-supplied
    `InstallAddonFunc`, but it walked the manifest in declared order.
    v3 `PresetAddon` now grows an optional `requires []string`; the
    resolver topologically sorts the merged required+optional list
    (Kahn's algorithm; preserves manifest order as the tie-breaker so
    legacy presets keep working unchanged) before handing it to
    `InstallPreset`. Cycles surface as `ErrCyclicDependency`; references
    to addons not in the preset surface as `ErrUnknownDependency` (the
    kernel does not model cross-preset deps — those are a host
    marketplace concern). Schema (both `manifest/v3/schema` and
    `docs/spec/v3`) accepts `requires` as an array of unique strings.
    The `POST /marketplace/install/preset` HTTP surface stays host-side
    — bundle download is a host concern (ops talks to hub, link has its
    own registry); the kernel only owns the orchestration primitive.

  - **Frontend hot-reload signal: structured asset payload.**
    `ManifestChangeEvent` grows three fields the SDK can drive a
    structured reload off: `Action` (`install`/`upgrade`/`uninstall`),
    `AssetPaths` (bundle-relative paths of every frontend asset
    materialised; SDK invalidates exact URLs without guessing) and
    `ContentHash` (SHA-256 over concatenated frontend bytes in
    sorted-path order, so "manifest hash flipped but the user-facing
    bundle didn't" — e.g. only `backend.wasm` rotated — skips a full
    federation reload). The `bridge` payload now carries the same trio
    over the existing `ADDON_MANIFEST_CHANGED` WebSocket topic, so the
    SDK consumer reads `evt.assetPaths.forEach(p => invalidate(p))`
    instead of guessing what to bust. Uninstall now also broadcasts so
    frontends drop their cached federated module.

### Migration notes

- Hosts embedding the kernel pick up i18n persistence automatically on
  next install — no code change required. The migration
  `0007_addon_i18n.up.sql` runs through whichever migration runner the
  host already uses; AutoMigrate keeps sqlite-backed tests working.
- Hosts that want WASM addons to be auto-loaded by the installer call
  `host.EnableWASM(ctx, caps)` exactly once at boot (no longer need to
  thread `host.LoadWASMFromBundle` through their install handler).
- Hosts wiring a custom `ManifestChangeBroadcaster` (anything other than
  the bridge's `WSManifestBroadcaster`) inherit the new event fields
  additively — legacy reducers that only read `oldHash`/`newHash`/`addonKey`
  continue to work.

## [0.20.0] - 2026-05-26

### Added

- **feat(manifest): `metadata.countries` regional catalog scoping (#92).**
  v3 manifests may declare `metadata.countries` (ISO 3166-1 alpha-2, e.g.
  `["MX"]`; empty = global). Schema + `v3.Metadata.Countries` +
  internal `Manifest.Countries` + `FromV3` mapping. The hub filters the
  catalog by the user's country so regional addons (fiscal: Carta Porte,
  SAT) do not surface where they don't apply. Round-trip test included.
  Additive.

## [0.19.0] - 2026-05-25

### Added

- **feat(manifest): `metadata.i18n` catalog localization (#91).** v3 manifests
  may declare `metadata.i18n` (a map `locale → {name, description, features}`).
  Distinct from the top-level `manifest.i18n` block (pointers to the app's
  string bundles) — `metadata.i18n` carries the marketplace copy inline so the
  hub can store and serve catalog metadata per locale. Schema +
  `v3.MetadataLocale` + internal `Manifest.MetadataI18n` + `mapMetadataI18n`
  in `FromV3`. Flat `name`/`description`/`features` remain the per-field
  fallback. Json tag is `metadata_i18n` internally to avoid collision with
  `Manifest.I18n`. Round-trip test included. Additive.

## [0.18.0] - 2026-05-25

### Fixed

- **fix(manifest): wasm action triggers validate without a `backend` block
  (v3) (#90).** `validateActionTrigger` required a wasm trigger's export to be
  present in `backend.exports` *always*, even when no backend block was
  declared. v3 manifests carry no backend block — their wasm handlers ARE the
  export surface — so an empty export set means "nothing authoritative to
  cross-check", not "the backend exports nothing". The membership check now
  runs only when a list is declared (`len(exports) > 0`), matching the
  pre-existing lenient behaviour of `validateLifecycleHooks`. Legacy manifests
  with explicit `Backend.Exports` keep the strict typo-check. Unblocks
  publishing first-party addons with native actions (pos:
  `open_session → OnSessionOpen`, etc.).

## [0.17.0] - 2026-05-25

### Added

- **feat(preset): resolve + install `kind: "Preset"` verticals as a unit
  (#89).** New `preset/` package that turns a `kind: "Preset"` v3 manifest
  (e.g. Pitsline) into an ordered addon list and drives an idempotent,
  per-addon install via a host-supplied `InstallAddonFunc` — the kernel never
  reinvents the single-addon installer, it only resolves the list and collects
  per-addon results. `ResolveFromManifest` / `Resolve` extract addons
  (required-first, optional flagged) + defaults from a parsed/raw v3 preset
  manifest. `InstallPreset` drives install per addon: idempotent (install func
  returns `false` ⇒ skipped "already installed"), optional addons gated by a
  caller selection set, optional failures reported but non-fatal, required
  failures abort by default (`ContinueOnRequiredError` opts out). Returns a
  `Summary` with `installed[]` / `skipped[]` / `failed[]`. Tests parse the real
  Pitsline manifest. Also: `bundle.Bundle.RawManifest` preserves verbatim
  `manifest.json` bytes. Additive.

## [0.16.0] - 2026-05-25

### Added

- **feat(v3): declarative line-items (repeatable group) action field (#88).**
  `ActionField` grows `ItemFields []ActionField` so a field with `type: "array"`
  can declare the columns of a repeatable line-items group (e.g. the item rows
  of a "Recibir mercancía" modal, or the debit/credit lines of a journal entry)
  directly in the manifest instead of needing a custom federated modal. Both
  schema copies (embedded + `docs/spec`) allow `item_fields` as an array of
  `ActionField` refs in the `ActionField` `$def`. `item_fields` has no legacy
  flat `FieldDef` slot, so it does NOT round-trip through `FromV3` — the SDK
  reads it off the v3-served action metadata (same pattern as
  widget/validation). Additive; flat-field rendering and the strict schema are
  unaffected.

## [0.15.0] - 2026-05-25

### Added

- **feat(manifest/v3): accept `Setting.description`, `Setting.type "number"`,
  `Column.comment`, handler `"compiled"` (#87).** The strict v3 contract
  (`additionalProperties:false` + `DisallowUnknownFields`) rejected legitimate
  optional metadata fields used by real addon manifests, making them
  un-installable. Added as additive/optional: `Setting.description` (human
  description of a tenant setting), `Setting.type` enum gains `"number"` (for
  decimal settings, e.g. a tax rate), `Column.comment` (column-level doc string
  on `models[].columns[]`), handler `type: "compiled"` (first-party native
  handlers) and a subscription `comment`. Both schema copies (embedded + docs)
  updated in sync. The new strings intentionally do NOT round-trip through
  `FromV3` (legacy structs have no slot — consumers read them off the v3-served
  metadata). Additive.

## [0.14.0] - 2026-05-25

### Added

- **feat(v3): action modals (fields/modal/confirm) + frontend federation
  (#86).** The v3 `Action` was a thin `{key, label, handler, target_model}` and
  the v3 `Manifest` had no frontend block, even though the legacy types, SDK
  and installer already supported rich declarative action modals, custom
  federated modals and federated frontends. This additively exposes them in the
  v3 contract:
  - v3 `Action` grows `Icon`, `Fields []ActionField`, `Modal`, `Confirm`,
    `ConfirmMessage`.
  - New `ActionField` (mirrors the SDK `ActionFieldDef` 1:1), `FieldOption`,
    `FieldValidation`.
  - v3 `Manifest` grows `Frontend *Frontend` (mirrors legacy `FrontendSpec`).
  - `FromV3.mapActions` copies `Icon`/`Confirm`/`ConfirmMessage`/`Modal` and
    folds each `ActionField` into a legacy `FieldDef`;
    `widget`/`validation`/`ref`/`placeholder`/`search_endpoint` have no
    `FieldDef` slot and intentionally do not round-trip (the SDK reads them off
    the raw manifest-served action metadata).
  - Embedded + docs JSON schema extended (`additionalProperties:false` +
    `DisallowUnknownFields` require declaring the new fields). Strictly
    additive — no existing field changed.

## [0.13.0] - 2026-05-25

### Added

- **feat(bundle): dual-read v2 and v3 manifests on ingestion (#85).**
  `bundle.Read` unmarshalled `manifest.json` directly into the legacy
  `manifest.Manifest`, so any v3-authored addon (all of `asteby-hq/addons`)
  failed to load. The reader now peeks the raw JSON for `apiVersion`: v3
  manifests are validated via `v3.Parse` and mapped into the legacy shape by a
  new `manifest.FromV3`, keeping `bundle.Bundle.Manifest` as `manifest.Manifest`
  so the ~14 existing consumers stay untouched. The legacy v2 path is
  unchanged. Adds a focused test loading the real inventory v3 manifest fixture.
- **feat(manifest): advertise kernel contract 3.0.0 now that v3 is ingested
  (#85).** The bundle path dual-reads Module Contract v3, so the kernel
  satisfies v3 addons' `kernel: ">=3.0.0 <4.0.0"` requirement. Bumped
  `manifest.APIVersion` 2.0.0 → 3.0.0. Legacy v2 addons declaring no kernel
  range are unaffected (`checkKernelRange` skips empty ranges).
- **feat(spec): freeze Module Contract v3 (#73).** The v3 manifest schema,
  migration guide and examples landed under `docs/spec/v3/`.
- **feat(auth): `AuthUserProvider` contract + adapters (modelbase, uuid-locals,
  jwt) (#76).** A pluggable provider so hosts that keep their own user index
  can drive the kernel's auth surface without registering each model in the
  package-init registry.
- **feat(query): relation filters, group_by, aggregations, preloads (#77).**
- **feat(marketplace): uninstall, upgrade, rollback, discovery endpoints (#74).**
- **feat(config,database): `OrgCurrencyGetter` + `WithFiberContext` helpers
  (#82).**
- **feat(database): `RegisterCurrencyDefaultCallback` BeforeCreate hook
  (#84).**
  Apps now wire a single callback on their root `*gorm.DB` that
  populates any model's `CurrencyCode` (or `Moneda`) string field at
  INSERT time from `config.OrgCurrencyGetter` resolved via the request
  context's `config.CurrencyGetterFromContext`. The hook reads
  `OrganizationID` (matching `modelbase.BaseUUIDModel`), resolves the
  per-org currency, and falls back to `database.CurrencyHookFallback`
  ("USD" — geography-agnostic) when no getter is wired or the org has
  no currency configured. Detection is by reflection of two recognised
  field names (`CurrencyCode`, `Moneda` — the latter for the SAT
  carta-porte CFDI model) and a struct-type cache keeps the hot path
  to a single `sync.Map` lookup per row. Explicit caller-supplied
  values are always preserved; batch inserts are handled per element.
  Motivation: the addon monorepo carried 18 `gorm:"default:'MXN'"`
  tags across customers / purchases / accounting-lite / waybill, kept
  as a DB-level fallback after Wave 2.5d's getter migration left no
  INSERT-time resolver. The hook lets addons drop the tag entirely,
  eliminating the MXN bias from a platform that ships in COP, USD,
  EUR, MXN, etc. New coverage in `database/currency_hook_test.go`
  exercises (a) getter resolves, (b) fallback when no getter, (c)
  fallback when getter reports miss, (d) explicit caller value wins,
  (e) `Moneda` field name, (f) no-op on models without a currency
  field, (g) batch insert with per-row org lookup, (h)
  `RegisterCurrencyDefaultCallback("")` defaults to USD, (i)
  idempotent registration.

### Fixed

- **fix(dynamic): `resolveModel` respects `Config.ModelResolver`.** The
  service's internal `resolveModel` helper (used by `List`, `Get`,
  `Create`, `Update`, `Delete`) called `modelbase.Get` directly,
  bypassing the configurable `ModelResolver` field. Hosts that keep
  their own model index (e.g. Ops via `meta-core/models`) could not
  drive dynamic CRUD without also registering each model in the
  package-init `modelbase` registry. `resolveModel` now routes
  through `Service.lookupModel`, which honours
  `Config.ModelResolver` when wired and falls back to `modelbase.Get`
  otherwise — so existing hosts keep working unchanged. New
  regression coverage in `dynamic/service_test.go` asserts (a) the
  custom resolver is invoked on every CRUD path, (b) the
  `modelbase.Get` fallback still works when no resolver is wired,
  and (c) a resolver returning `(nil, false)` surfaces
  `ErrModelNotFound` without silently falling through.

## [0.12.0] - 2026-05-18

### Security

- **fix(security): extend AST gating to `RangeTblFunc` (XMLTABLE /
  JSON_TABLE).** PR #61 / PR #63 closed cross-schema reads through
  `RangeVar` and `RangeFunction`, but libpg_query exposes the
  `XMLTABLE` and `JSON_TABLE` SQL constructs as their own
  `RangeTableFunc` and `JsonTable` nodes — distinct from both. A
  guest writing
  `SELECT * FROM XMLTABLE('//row' PASSING (SELECT data FROM other.audit_log) COLUMNS …)`
  (or the `JSON_TABLE` counterpart) could therefore read another
  schema through the PASSING subquery without the walker ever
  surfacing the reference. The `runtime/wasm` AST walker now descends
  through `RangeTableFunc` (`Docexpr` / `Rowexpr` / namespaces /
  `RangeTableFuncCol` `Colexpr` + `Coldefexpr`), `JsonTable`
  (`ContextItem` / `Passing` / `Columns`), `JsonTableColumn` (nested
  `NESTED PATH` children plus `OnEmpty` / `OnError` DEFAULT
  expressions), `JsonValueExpr`, `JsonArgument`, `JsonBehavior` and
  `RangeTableSample` so any nested `RangeVar` / `RangeFunction` /
  `RangeTableFunc` / `JsonTable` reachable from those constructs
  trips the surrounding capability gate. The XML/JSON constructs
  themselves are SQL syntax (not user-defined functions) so there's
  no function name to gate at the construct level — only the
  expression children carry attackable references. Inside a DML
  scope (`INSERT … SELECT … XMLTABLE(…)`) the read entries are gated
  under `db:read` exactly like an `UPDATE … FROM` source; unlike
  `RangeFunction` the XML/JSON constructs do not also trigger the
  defensive both-axes gate because they're built-in SQL, not
  arbitrary `setof`-returning code. New coverage in
  `runtime/wasm/dbquery_rangetblfunc_test.go` (14 cases — extractor
  unit tests plus full-stack `executeDBQuery` / `executeDBExec`
  integration tests with sqlmock). Doc: `docs/wasm-abi.md` § 9.3 and
  § 10.3 list `RangeTableFunc` / `JsonTable` in the enforcement
  contract. Patch bump — purely additive defence-in-depth, no ABI
  change.

### Added

- **feat(guest): complete TinyGo helpers for `db_query`, `db_exec`,
  `http_fetch`, `env_get`, `log`.** Closes out the helper matrix the
  initial `EmitEvent` PR (#66) called out as roadmap in
  `docs/guest-go.md` § 5. Each helper ships in the same two-file
  pattern as `EmitEvent`: a host-runnable `<name>.go` with the typed
  result struct, typed error and envelope decoder + a
  `//go:build wasm || wasip1` `<name>_wasm.go` carrying the
  `//go:wasmimport metacore_host <name>` stub and the (ptr, len)
  marshalling. `Log(level, msg)` adds a guest-side `[<level>]` prefix
  so the host's existing single-string log import surfaces severity
  without an ABI change. `EnvGet(key)` returns `(value, found, err)`
  and folds "missing key" + "empty value" into a single
  `found == false` (mirrors `runtime/wasm/capabilities.go` line
  134-137); the third slot is reserved for a future typed envelope.
  `HttpFetch(req)` decodes the flat `{status, body}` success shape
  and the `{error, message}` failure shape the host emits via
  `jsonError`, and surfaces `forbidden` as a dedicated
  `*HttpCapabilityDeniedError` for `errors.As` branching. `DbQuery`
  and `DbExec` share the `{success, data, meta}` decoder skeleton —
  `DbExec` adds a `SQLState` field on its typed error so driver-
  level violations (`constraint_violation` /
  `serialization_failure`) carry the Postgres SQLSTATE through to
  the guest. Each helper has an envelope-decoder test matrix (happy
  path, typed error paths, empty / null buffer, malformed JSON) —
  all run host-side with `go test ./guest/...` (no WASM required).
  `docs/guest-go.md` is updated to document all six helpers with
  sample usage; the "roadmap" section is removed. Purely additive —
  addons that hand-rolled the raw ABI keep working.

- **`installer.Upgrade` — fire-site for the `upgrade` lifecycle event.**
  Before this release the `upgrade` event existed as a constant
  (`lifecycle.HookEventUpgrade`) and validated as a manifest key, but no
  caller ever fired it: addon authors who declared
  `lifecycle_hooks.upgrade` never saw a dispatch because the installer's
  only path forward was `Install` (which treats every call as a fresh
  install). The new `Installer.Upgrade(ctx, orgID, newBundle)` method
  drives a full upgrade transition:
  1. Verifies the new bundle's signature and re-runs manifest
     `ValidateAdvisory`.
  2. Loads the existing `metacore_installations` row; returns
     `ErrNotInstalled`, `ErrSameVersionUpgrade`, or `ErrCannotDowngrade`
     before any DB mutation when the preconditions fail.
  3. Dispatches `lifecycle_hooks.upgrade` with `phase: "before"` and a
     payload that carries `from_version` / `to_version`. A non-nil error
     aborts: the row is untouched and no schema work runs.
  4. Calls `dynamic.EnsureSchema` → `dynamic.Apply` → CreateTable /
     SyncSchema on the new manifest (additive; old columns survive).
     `dynamic.Apply` is idempotent — already-recorded migrations are
     skipped and only new files execute.
  5. Re-projects manifest CRUD hooks into `dynamic.HookRegistry`
     (same `UnregisterAddon` + `RegisterManifestHooks` round-trip that
     `Install` uses, so the new before_*/after_* shape replaces the old
     one without doubling up).
  6. Persists the version bump with a settings merge — user-tuned values
     win; new defaults declared by the new manifest are added.
  7. Dispatches `lifecycle_hooks.upgrade` with `phase: "after"`, a
     `migrations_applied` counter, and the same `from_version` /
     `to_version` payload. AFTER errors are logged and swallowed (the
     upgrade has committed; DDL rollback is unsafe).
  8. Broadcasts a `ManifestChangeEvent` so SDK frontends drop their
     metadata cache without polling.
  Companion HTTP endpoint: `PUT /api/metacore/installations/:key/version`
  on `httpx/metacore/handler.go`, mirroring the existing `Install` route
  shape (multipart bundle upload, key match check, sentinel-to-HTTP-code
  mapping for the three guard errors). Companion doc:
  [`docs/lifecycle-hooks.md` § 3](docs/lifecycle-hooks.md) now documents
  the enriched payload and the guard rails. Minor bump — purely additive.

## [0.11.0] - 2026-05-14

### Added

- **ABI WASM v1.0 contract frozen + documented (PR #56).** The full host
  import / export contract, manifest schema, capability axes, and host
  envelope shapes are now codified in
  [`docs/wasm-abi.md`](docs/wasm-abi.md) as the v1.0 stable surface that
  third-party addon authors can target. The audit pass that produced the
  freeze flagged seven follow-up inconsistencies (resolved across PRs
  #58 / #61 / #62 / #63 / #64 / #65 in this release) so the doc and the
  runtime line up byte-for-byte for v1.0. Future ABI versions will bump
  `meta.envelopeVersion` (host envelopes) and `wasm.ABIVersion` (kernel
  Go constant) in lockstep; v1 stays the supported floor for the
  foreseeable future.
- **`hub/` package — kernel-side Hub HTTP client for ecosystem consumers
  (PR #57).** New `github.com/asteby/metacore-kernel/hub` package that
  speaks the Hub catalog/bundle contract end-to-end so every kernel
  consumer (ops, link, custom apps embedding the kernel) gets browse →
  fetch → install with one import path. API: `hub.NewClient(baseURL,
  token, opts...)`, `hub.FromEnv(opts...)` (reads `HUB_BASE_URL` /
  `HUB_LICENSE_TOKEN`), `c.FetchCatalog(ctx, params) → CatalogResult`,
  `c.FetchAddon(ctx, key) → *AddonDetail` (`errors.Is(err,
  ErrNotFound)`), `c.DownloadBundle(ctx, key, version) → io.ReadCloser`
  (streamed, never buffers — wires directly into `kernel/bundle.Read`),
  and `c.FetchSpec(ctx) → *Spec` (`ErrSpecUnavailable` when `/v1/spec`
  404s). Bearer auth is optional: public catalogs browse anonymously,
  license token is sent as Bearer when configured. Zero kernel-internal
  dependencies so non-kernel Go services (Hub admin tooling, CI bots)
  can import the package standalone. Tests in `hub/client_test.go` are
  fully self-contained — httptest server speaks the Hub contract,
  covers happy paths, 404 → `ErrNotFound`, `/v1/spec` →
  `ErrSpecUnavailable`, header / query forwarding, and a 1 MiB stream
  regression for `DownloadBundle`. Minor bump — purely additive, no
  kernel-internal changes.
- **`metacore_host.event_emit` now returns the canonical
  `{success, data, meta}` envelope documented in
  [`docs/wasm-abi.md` § 12.4](docs/wasm-abi.md).** Prior to this release
  the host import returned literal `0` on success and a bare
  `{"error","message"}` JSON on failure (`runtime/wasm/capabilities.go:202`),
  diverging from the doc — the divergence was the inconsistency #4 of the
  ABI v1.0 freeze audit. The new envelope adds `data.event`,
  `data.subscribers`, `meta.addon`, `meta.orgId`, `meta.emittedAt`,
  `meta.durationMs`, and a versioned `meta.envelopeVersion` (`1` today,
  exposed in Go as `wasm.EventEmitEnvelopeVersion`). The change is
  **wire-compatible** with legacy guests that ignored the `i64` return
  value: the publish side-effect runs before the host writes the envelope,
  so dropping the return packs into a `drop; i64.const 0` body keeps
  working (regression test `TestHost_InvokeEventEmitLegacyGuestIgnoresReturn`).
- **`events.Bus.PublishWithCount(ctx, addonKey, event, orgID, payload)
  (int, error)`** — count-returning sibling of `Publish`. The wasm host
  import uses it to populate `data.subscribers`; `Publish` is kept as a
  thin one-line wrapper for source compatibility.
- **`manifest.ValidateAdvisory(kernelVersion) ([]string, error)` — non-fatal
  warnings alongside the strict error.** The ABI v1.0 freeze audit
  (PR #56 § 12) flagged three manifest fields that validate but either
  duplicate a now-canonical surface or are reserved for a future runtime.
  The advisory entry-point surfaces those cases without breaking existing
  manifests: `Validate` keeps its current signature and error contract,
  `ValidateAdvisory` returns the same first error and an extra slice of
  warnings. The installer now calls the advisory path and logs each
  warning via `slog.Warn("manifest.advisory", …)` so operators see the
  drift in the boot trail. See `docs/wasm-abi.md` § 13 for the full
  catalogue of advisory cases.
- **`db_exec` now surfaces `RETURNING` rows on the success envelope.**
  Prior to v0.11.0 every mutation routed through `gorm.Exec`, which
  discards the rows a `RETURNING` clause produces — the guest only saw
  `rowsAffected` even though `docs/wasm-abi.md` § 10.4 advertised
  `data.rows`. The kernel now detects `RETURNING` (regex over stripped
  literals, same rules as `validateMutationOnly`) and routes the
  statement through `gorm.Raw().Rows()` so every returned row reaches the
  guest. Envelope shape mirrors `db_query`: `data.rows`, `data.columns`,
  `data.rowsAffected = len(rows)`. Statements without `RETURNING` keep
  the legacy envelope (`rowsAffected` only) verbatim — the change is
  purely additive and ABI-compatible. The kernel row cap
  (`dbQueryMaxRows = 10_000`) applies to `RETURNING` results too;
  exceeding it returns `row_limit_exceeded`. Covered by
  `TestExecuteDBExec_InsertReturningIncludesRows`,
  `TestExecuteDBExec_UpdateReturningStarIncludesRows`,
  `TestExecuteDBExec_InsertWithoutReturningOmitsRows`,
  `TestExecuteDBExec_ReturningLiteralDoesNotTrigger` and
  `TestContainsReturning`.
- **`host.ErrRuntimeNotImplemented` — clear failure for
  `backend.runtime = "binary"`.** The reserved runtime value validated
  silently but `host.LoadWASMFromBundle` returned `nil` for any non-wasm
  runtime, which let a binary-runtime addon install as a dead bundle.
  v0.11.0 surfaces a wrapped sentinel error so call sites can branch with
  `errors.Is(err, host.ErrRuntimeNotImplemented)` and `ValidateAdvisory`
  flags the same case at authoring time.
- **`manifest.lifecycle_hooks` is now functional.** The field was declared
  in `manifest/manifest.go` and validated structurally, but no consumer
  read it — addons could declare hooks that never fired (flagged by the
  ABI v1 freeze audit as inconsistency #1 / "reserved"). v0.11.0
  implements the read + dispatch path end-to-end so the contract matches
  the addon authoring docs:
  - New `lifecycle.HookRunner` (`lifecycle/hooks.go`) reads
    `manifest.LifecycleHooks`, sorts each event's `HookDef` slice by
    `Priority` ascending, and dispatches through registered
    `HookDispatcher`s keyed by `HookTarget.Type` (`wasm` | `webhook` |
    `prompt`). Mirrors the `dynamic.ActionDispatcher` pattern that
    already ships for `manifest.actions`.
  - `installer.Installer.HookRunner` and `installer.Installer.DynamicHooks`
    are new optional fields. `Install` fires the `install` and `enable`
    hooks; `Enable` fires `enable`; `Disable` fires `disable`;
    `Uninstall` fires `disable` + `uninstall`. The reserved `upgrade`
    event is recognised by validation today and will be fired by future
    upgrade flows.
  - `dynamic.HookRegistry.RegisterManifestHooks(addonKey, manifest, invoker)`
    projects every `before_*` / `after_*` declaration into the existing
    per-model hook chains so `dynamic.Service.Create/Update/Delete`
    fires them automatically. `UnregisterAddon` rips the registration
    out wholesale on uninstall so reinstalls stay idempotent.
  - `manifest.Validate` now enforces the closed set of events (`install`
    | `uninstall` | `enable` | `disable` | `upgrade` | `before_create`
    | `after_create` | `before_update` | `after_update` | `before_delete`
    | `after_delete`), the closed set of target types, the wasm-export
    cross-check, the webhook-URL requirement, and the
    `async`-forbidden-on-before-events rule.
  - `host.AppConfig.EnableLifecycleHooks` opts a host in. When true,
    `NewApp` constructs the runner and registry, exposes them on
    `app.HookRunner` / `app.DynamicHooks`, and wires the registry into
    `dynamic.Service`. `host.Config.HookRunner` / `host.Config.DynamicHooks`
    forward the same instances into the installer so `Install` /
    `Enable` / `Disable` / `Uninstall` dispatch the lifecycle hooks
    automatically. Hosts still register their wasm + webhook
    dispatchers explicitly because those depend on the host's wazero
    instance and signed webhook dispatcher.

  Error semantics are documented at
  [`docs/lifecycle-hooks.md`](docs/lifecycle-hooks.md): lifecycle events
  and `before_*` hooks abort the operation on error; `after_*` hooks log
  and continue so a flaky notification never strands a committed row.
  `async: true` is allowed only on after-events because before-events
  must block on the result to honour a veto.

  Existing apps are unaffected — `LifecycleHooks` is opt-in by manifest
  authors and the host runner is opt-in by app config. Apps that do not
  declare hooks or do not enable the runner keep their pre-v0.11.0
  behaviour byte-for-byte.

  Tests: `lifecycle/hooks_test.go`, `manifest/validate_test.go`
  (TestValidate_LifecycleHooks_*), `dynamic/hooks_test.go`,
  `installer/lifecycle_hooks_test.go`. Minor bump — additive feature, no
  breaking changes.

- **New `runtime/flow` package — generic workflow engine extracted from
  link.** The kernel now ships a DAG executor with a pluggable node
  registry, template interpolation, optional persistence (`Store` interface),
  optional progress notifications (`ProgressSink`), and a `TriggerService`
  coordinator that routes incoming events to matching flows. Built-in
  domain-free node executors cover HTTP, Webhook, Condition, Switch, Delay,
  Loop, Filter, SetVariable, TransformData, Split, Merge, ErrorHandler,
  Note, and Trigger. Apps register their own domain nodes
  (`message`, `ai_chat`, `create_ticket`, …) via `Engine.RegisterNode`.
  An optional Fiber `Handler` exposes the runtime endpoints
  (`POST /:id/run`, `POST /:id/test`, `POST /:id/cancel`); flow CRUD stays
  with the host because flows live in host-specific tables. The kernel
  does NOT persist flows, expose CRUD, or know about contacts / tickets /
  messaging / AI — those remain app-side concerns. See
  [`docs/flow.md`](docs/flow.md) for the DSL spec. This is the additive
  half of the flow extraction; link will migrate to consume
  `runtime/flow` in a follow-up PR and delete its `internal/flow`
  vendor copy then. Additive, no breaking changes.

- **`guest/` package — TinyGo-compatible helpers for addon authors
  (PR #66).** New package `github.com/asteby/metacore-kernel/guest`
  consumable from any wasm backend (TinyGo target `wasi` /
  `wasm-unknown`). The first helper, `guest.EmitEvent(event, payload)`,
  wraps the `metacore_host.event_emit` import: marshals the payload via
  `encoding/json`, calls the host, unpacks the `(ptr<<32)|len` return
  value, reads the response buffer out of guest memory, and decodes the
  `{success, data, meta}` envelope documented in
  [`docs/wasm-abi.md` § 12.4](docs/wasm-abi.md#124-response-envelope)
  into a typed `EmitEventResult` plus typed `*EmitEventError`.
  Forward-compatible decoder: unknown `meta.envelopeVersion` values are
  tolerated (known fields populated, unknown fields ignored) so guests
  authored against v1 keep working when the host bumps to v2. Empty-
  buffer return (`packed == 0`) is preserved as a zero-value success so
  the helper is also compatible with hosts on the pre-PR-#62 contract
  that returned literal `0` on success. The package additionally ships
  an opt-in default `alloc` export (build tag `metacore_guest_alloc`)
  for addons that don't already define their own bump allocator. Only
  stdlib used (`encoding/json`, `errors`, `unsafe`); no kernel-runtime
  dependencies pulled in, so TinyGo can compile the helpers cleanly.
  See [`docs/guest-go.md`](docs/guest-go.md) for usage. Follow-up to
  PR #62 (`event_emit` rich envelope) which called out "guest SDK
  helpers, not the kernel" as the next step. Purely additive.

### Fixed

- **`runtime/wasm` now propagates `orgID` end-to-end into `event_emit`
  publishes (PR #58).** The `invocation.orgID` field declared on
  `runtime/wasm/capabilities.go` was never populated by `invokeImpl`, so
  every guest call to `event_emit` reached `events.Bus.Publish` with
  `uuid.Nil`. Subscribers filtering by tenant (the documented contract,
  see `events/events.go:Handler`) silently saw cross-org bleed because
  the bus does not differentiate publishers — it forwards whatever
  `orgID` the caller passed. Audit item #5 of the ABI v1 freeze flagged
  this. Resolution: a new `runtime/wasm.WithOrgID(ctx, orgID)` context
  helper carries the tenant id without changing any public Invoke
  signature, and two ergonomic siblings
  (`Host.InvokeFor(ctx, orgID, ...)`, `Host.InvokeInTxFor(ctx, tx,
  orgID, ...)`) make the binding explicit at call sites. `invokeImpl`
  reads the orgID off ctx when building the per-invocation bag, so any
  caller that wraps with `WithOrgID` lights up automatically.
  `host.Host.InvokeWASMFor` mirrors the new entry at the kernel-facade
  layer. `event_emit` now also enforces the documented `no_active_org`
  guard — a publish without a bound orgID returns the JSON error
  envelope instead of silently fanning out under `uuid.Nil`. The full
  contract lives at `docs/wasm-abi.md` § 12.6. Additive and backwards
  compatible: legacy callers that go through `Host.Invoke` keep working
  for every non-tenant import (log, env_get, http_fetch, db_query,
  db_exec); only `event_emit` from a tenantless context surfaces
  `no_active_org`, which is the correct behaviour.
- **`runtime/wasm.db_query` now enforces the documented cross-schema
  capability gate via an AST walk (PR #61).** The pre-fix path only ran
  `Enforcer.CheckCapability(addonKey, "db:read", "addon_<key>.*")` against
  the addon's own implicit schema. `SET LOCAL search_path TO addon_<key>,
  public` did scope bare names back into the addon schema, but a guest
  writing `SELECT * FROM public.users` or `SELECT * FROM other_addon.foo`
  bypassed the gate entirely — capability declarations like
  `db:read public.invoices` were optional in practice. Documented as a
  soft-guarantee in [`docs/wasm-abi.md`](docs/wasm-abi.md) § 9.3; flagged
  as hallazgo #6 of the ABI v1 audit and closed before opening the kernel
  to third-party addons. Resolution: every `db_query` payload is now
  parsed with libpg_query (via `github.com/pganalyze/pg_query_go/v6`) and
  the walker pulls every `RangeVar` reachable from the AST — top-level
  FROM tables, JOIN arms, CTE bodies, `RangeSubselect` derived tables,
  `IN(SELECT …)` SubLinks, UNION / INTERSECT / EXCEPT arms and `VALUES`
  rows. Each reference is then gated individually against the addon's
  compiled capability list (`db:read <schema>.<rel>` or
  `db:read <schema>.*`); the `pg_catalog` / `information_schema` /
  `pg_*` meta-schemas are denied at the AST layer regardless of grants,
  giving us a defence-in-depth twin to the string-level filter in
  `validateSelectOnly`. A parse failure rejects the statement with
  `invalid_sql` instead of silently bypassing the gate. The `db_query`
  host import signature is unchanged — this is purely an internal
  hardening, ABI v1 stays stable. New test file
  `runtime/wasm/dbquery_xschema_test.go` covers bare-name still allowed,
  own-schema qualified still allowed, cross-schema without cap denied,
  cross-schema with `<schema>.*` cap allowed, cross-schema with
  per-relation cap allowed, mixed cross-schema joins (partial cap =
  deny, full cap = allow), CTE / SubLink / RangeSubselect-hidden
  references denied, and a parse-error regression that asserts we
  reject instead of degrade. Only new dep is
  `github.com/pganalyze/pg_query_go/v6` (CGO, already required for
  `mattn/go-sqlite3`); the sole new transitive is
  `google.golang.org/protobuf` which we already pull through
  `prometheus/client_golang`. Security hardening, no consumer-facing
  changes.
- **`runtime/wasm.db_exec` now enforces the documented cross-schema
  capability gate via the same AST walk introduced for `db_query`
  (PR #63).** The pre-fix `db_exec` path only ran
  `Enforcer.CheckCapability(addonKey, "db:write", "addon_<key>.*")`
  against the addon's own implicit schema. `SET LOCAL search_path TO
  addon_<key>, public` did scope bare names back into the addon schema,
  but a guest writing `UPDATE public.users SET role = 'admin'` (or any
  other cross-schema `INSERT` / `DELETE` / `MERGE`, or a CTE-hidden
  mutation like `WITH x AS (UPDATE other.t SET …) …`) bypassed the gate
  entirely — declarations like `db:write public.users` were optional in
  practice. Documented as the soft-guarantee twin of § 9.3 in
  [`docs/wasm-abi.md`](docs/wasm-abi.md) § 10.3; flagged as a hallazgo
  follow-up to PR #61 before opening the kernel to third-party addons.
  Resolution: the `extractRelations` walker from PR #61 is generalised
  into `extractMutationRelations`, which still parses with libpg_query
  but now tags each relation reference by the capability axis it must
  satisfy. DML targets (the `Relation` field of an `InsertStmt` /
  `UpdateStmt` / `DeleteStmt` / `MergeStmt` at any nesting depth,
  including inside a `WITH … <DML>` CTE body) emit a `db:write` ref;
  read-only sources (`UPDATE.FROM`, `DELETE.USING`, `MERGE` source,
  `INSERT.SELECT`, `RETURNING` / `WHERE` subqueries, CTE bodies that
  are themselves SELECTs) emit `db:read` refs. Each is gated
  individually against the addon's compiled capability list. The
  `validateMutationOnly` string-level check is loosened to accept a
  leading `WITH` (Postgres allows `WITH cte AS (…) INSERT / UPDATE /
  DELETE / MERGE`); the AST-layer top-level check inside
  `extractMutationRelations` enforces that the top-level statement
  after the WITH is a DML so `db_exec` can't be used as a SELECT bypass.
  Parse failure rejects with `invalid_sql` instead of degrading to
  "permit".
- **`db_query` and `db_exec` walkers now gate `RangeFunction` references
  (function-as-table — `SELECT * FROM other.fn(args)`) (PR #63).** The
  PR #61 walker only reached `RangeVar` nodes, so libpg_query's split
  between table references (`RangeVar`) and function references
  (`FuncCall` wrapped in `RangeFunction.Functions[]`) left an open
  vector: a guest could read or call `other_addon.my_func()` without
  declaring any capability for `other_addon.*`. The walker now recurses
  into `RangeFunction.Functions[]`, pulls the schema-qualified function
  name out of `FuncCall.Funcname` (Postgres lists this as `[schema, fn]`
  or `[catalog, schema, fn]` — we collapse to `schema.fn`), and gates
  the reference exactly like a relation: bare names ride the implicit
  own-schema grant through `SET LOCAL search_path`; cross-schema names
  require an explicit `<cap> <schema>.<fn>` (or `<cap> <schema>.*`)
  declaration. Inside a mutation scope a `setof`-returning function may
  read AND write state, and the AST gives us no way to tell which, so
  the function name is gated **defensively under both axes** —
  declared caps must cover `db:read <schema>.<fn>` and
  `db:write <schema>.<fn>` to let the call through.
- **Tests.** New file `runtime/wasm/dbexec_xschema_test.go` covers the
  PR #63 regressions end-to-end: bare-table allowed, own-schema
  qualified allowed, `UPDATE public.users` without cap denied,
  `UPDATE public.users` with `db:write public.*` allowed, with
  `db:write public.users` allowed, `INSERT` / `DELETE` / `MERGE` into
  another schema without cap denied, `UPDATE … FROM cross.schema`
  source side gated under `db:read`, CTE-hidden DML cross-schema
  denied, `WITH … SELECT` rejected at the AST layer, parse-error
  regression (`invalid_sql` not "permit"), `db_query`
  `SELECT * FROM other.my_func()` without cap denied / with cap
  allowed, and `db_exec`'s defensive function-as-table dual-axis gate.
  `extractMutationRelations` also gets a focused unit-test layer for
  every DML node shape so the cap tagging stays explicit. No
  public-API changes — `executeDBExec` / `executeDBQuery` signatures
  and the host-import wire shape are byte-for-byte identical, and the
  `db_exec` envelope is unchanged.

### Deprecated

- **`manifest.events`** — kept for back-compat; will be derived from
  `Capabilities[event:emit]` in v2. `ValidateAdvisory` emits a warning
  for every entry that has no matching `event:emit` capability so
  addon authors can migrate before v2 removes the field. The runtime
  authoritative gate has been the capability set since v0.8 — the array
  itself was never consulted.

### Notes

- No breaking changes to the `event_emit` import signature. ABI v1
  remains intact — only the return-value semantics changed (from "0 on
  success" to "ptr|len of envelope always"), and the publish side-effect
  ordering is unchanged.
- The audit cross-reference at
  [`docs/audits/2026-05-04-host-functions-gap.md`](docs/audits/2026-05-04-host-functions-gap.md)
  has been amended with the resolution note.

## [0.10.1] - 2026-05-11

### Added

- **`host.App` now wires the in-process `events.Bus` into `dynamic.Service`
  so canonical CRUD events fan out to subscribers post-commit.** Previously
  `host.NewApp` constructed `dynamic.New(...)` without a `Bus`, which turned
  every `publishCanonical` call inside the dynamic engine into a silent
  no-op — the bus support landed in `dynamic.Service` in v0.10.0 but was
  never reached by hosts going through `host.NewApp`. The audit at
  [`docs/audits/2026-05-04-dynamic-events.md`](docs/audits/2026-05-04-dynamic-events.md)
  flagged the missing wire-in; this change closes it. `host.App` now owns
  a single `*events.Bus` (exposed as `app.Bus`) constructed inside
  `NewApp` and shared with `dynamic.Service` via the existing
  `dynamic.Config.Bus` field. Two new optional fields on `AppConfig` let
  hosts tune the wiring without touching the kernel:
  `EventsEnforcer *security.Enforcer` (capability gate; nil disables
  enforcement, kernel-originated publishes bypass it regardless) and
  `AddonKeyForModel func(ctx, model) string` (namespaces the canonical
  event under `<addonKey>.<model>.<action>`; nil falls back to
  `"kernel"`). Both default to nil so existing apps upgrade transparently
  and immediately gain the canonical event stream under the `kernel.*`
  namespace. Tests in `host/app_test.go` cover the regression
  (`TestApp_BusWiredIntoDynamic`) and the resolver path
  (`TestApp_AddonKeyForModelResolverFlowsThrough`). Minor bump — additive
  and backwards compatible.

## [0.10.0] - 2026-05-11

### Breaking

- **`GET /api/options/:model?field=…` now returns the canonical
  `{success, data, meta}` envelope (v0.9.0).** The previous shape was
  `{success, data, type}` — the static/dynamic discriminator rode at the
  envelope root which forced consumers to special-case this single route.
  As of v0.9.0 the discriminator is nested under `meta.type` (alongside
  `meta.count`) so the response matches every other dynamic handler. Any
  client reading `response.type` must now read `response.meta.type`.
  See SDK ≥ next-minor (`@asteby/metacore-runtime-react useOptionsResolver`)
  for the upgraded consumer; bridge clients must adjust the unmarshal
  target.

### Added

- **`installer.ManifestChangeBroadcaster` — push hot-swap signals over the
  WebSocket hub without polling.** The installer now snapshots a manifest's
  SHA-256 fingerprint on every install and, when it differs from the
  previously persisted hash (or when no row existed yet, signalled by an
  empty `OldHash`), invokes the configured broadcaster with a
  `ManifestChangeEvent`. The kernel ships a `NoopBroadcaster` default so
  hosts that do not surface a real-time channel keep working unchanged;
  `Installer.WithBroadcaster` swaps the implementation following the Law 2
  pluggable-default pattern. A new `Installation.ManifestHash` column is
  populated on every install (legacy rows pre-dating the column are
  auto-populated on the first reinstall). Broadcast errors are logged and
  never roll back the install — the WebSocket fan-out is best-effort. The
  installer never imports `kernel/ws` directly: the seam lives in
  `bridge/installer_broadcaster.go`, which wraps a `*ws.Hub` into the
  interface and emits `ws.MessageType("ADDON_MANIFEST_CHANGED")` per-user
  for every operator of the affected org. Frontends consuming
  `@asteby/metacore-runtime-react` can drop the payload straight into
  their metadata-cache invalidation reducer. Minor bump — additive,
  backwards compatible.
- **`FrontendSpec.Layout` — opt-in immersive (full-page) addon UI.** Manifests
  can now declare how the host should frame the federated module on screen.
  Two values are recognised: `"shell"` (the default chrome — sidebar + topbar
  + content slot, identical to today's behaviour) and `"immersive"` (the
  addon owns the entire viewport, no sidebar or topbar). Empty / unset is
  interpreted as `"shell"` so every manifest published before this release
  validates and renders unchanged. `manifest.Validate` rejects any other
  value so typos surface at install time rather than at first paint. The
  field unlocks kiosk-style surfaces (POS terminals, kitchen-display
  screens, customer-facing waiting-room displays) on top of the same kernel
  contract that powers chrome-wrapped admin pages. This is a minor bump —
  the field is additive, optional and backwards compatible.
- **`ColumnDef.Ref` auto-derivation from `belongs_to` relations.** Models
  that implement `modelbase.HasRelations` (compiled core models) and addon
  manifests that declare `RelationDef{Kind: "belongs_to", …}` now get
  `Ref` stamped on every FK column whose name matches the relation's
  `ForeignKey`. Author-provided `Ref` values always win — the inference is
  purely additive. Compiled models go through `metadata.Service` (runs
  inline in `computeTable`, no extra `modelbase.Get` round-trip per
  request); addon manifests can call `manifest.AutoDeriveColumnRefs(def)`
  during install if the host wants the same behaviour pre-projection. New
  relation kind `"belongs_to"` is whitelisted by `manifest.Validate` —
  `Pivot` is rejected for it (same shape as `one_to_many`).
- **Locale-aware validators via `$org.<key>` references.** `ColumnDef.Validation.Custom`
  and `FieldDef.Validation` now accept a `$org.<key>` token in addition to
  the legacy dotted identifier. The metadata service resolves the
  reference at request time through an app-supplied `OrgConfigResolver`
  registered with `metadata.Service.WithOrgConfigResolver`. Unresolved
  references pass through untouched so the SDK can decide between
  fallback validators and surfacing the missing config to operators.
  This is the contract that keeps fiscal/regional rules (RFC México, NIT
  Colombia, RUC Perú, etc.) out of core: the kernel only knows how to
  swap a token for the validator identifier the org actually wants
  applied. Cache is bypassed when an org resolver is registered because
  resolution varies per-request.
- **`modelbase.RelationDef` and `modelbase.HasRelations`.** Mirrors of the
  manifest types so compiled core models and declarative addons share one
  vocabulary. The metadata service consumes `HasRelations` to power the
  Ref auto-derivation described above.
- **`modelbase.ColumnDef.Ref`, `modelbase.ColumnDef.Validation`,
  `modelbase.FieldDef.Ref`.** New optional fields exposed through the
  `/api/metadata/table/:model` payload so the SDK's `<DynamicForm>` and
  `<DynamicRelation>` can build reference-aware selects without per-page
  wiring.

### Added (pre-existing)

- **Per-file SHA-256 verification on bundle install
  (`installer.verifySignature` / `security.VerifyBundle`).** When the
  publisher stamps `manifest.Signature.Checksums` (a map keyed by in-archive
  path), the kernel now compares each declared digest against the SHA-256 of
  the corresponding entry as it was actually read from the tarball. The check
  runs only after the global Ed25519 signature already verifies — Ed25519
  remains the load-bearing supply-chain guarantee, the per-file pass adds the
  granularity the audit
  [`docs/audits/2026-05-04-bundle-checksums.md`](docs/audits/2026-05-04-bundle-checksums.md)
  flagged as missing (post-mortem diagnostics, partial / streaming
  verification, per-asset rotation). Mismatch, missing entry (declared in
  `Checksums` but not present in the bundle) and extra entry (present but
  not declared) all surface as the new `security.ErrChecksumMismatch` with
  the offending path in the message. `manifest.json` is excluded from the
  per-file check on both sides because it carries the `Checksums` map and
  cannot self-checksum without a fixpoint cycle — its integrity is covered
  transitively by the Ed25519 over the full tarball. Bundles published
  before this release leave `Checksums` empty and are accepted unchanged
  (legacy compat). `bundle.Bundle` gains an `EntryDigests map[string]string`
  field, populated transparently by `bundle.Read` for every regular entry.
  Tests cover happy path with checksums populated, single-entry tampering
  (named offender in error), missing checksum target, undeclared extra entry
  and the legacy empty-map path; an installer-level test exercises the same
  flow through `verifySignature` end-to-end.
- **`POST /dynamic/:model/:id/action/:key` — per-row action endpoint.** The
  dynamic handler now mounts an action route alongside the CRUD verbs.
  `Service.ExecAction` runs the four-step contract from
  [`docs/dynamic-actions.md`](docs/dynamic-actions.md): load the row through
  `service.Get` (org-scoped) → optionally open `db.Transaction` when
  `Trigger.Type=="wasm" && Trigger.RunInTx` → dispatch to a registered
  `ActionDispatcher` keyed by `Trigger.Type` → commit on `Success=true` /
  rollback on `Success=false` (sentinel error consumed inside the call) →
  reply with the kernel envelope `{success, data, meta}` (`error` block on
  failure). Wired via three new `dynamic.Config` fields:
  `ActionResolver func(ctx, model, key) (*manifest.ActionDef, bool)` (the
  kernel does not own a global Actions index — hosts plug their addon
  registry here), `ActionDispatchers map[string]ActionDispatcher` (one per
  `Trigger.Type`; `wasm` and `webhook` must be wired by the host to keep
  `dynamic` free of `runtime/wasm` / `net/http` imports), and an
  auto-registered built-in `NoopDispatcher` for `Trigger.Type=="noop"`
  (UI-only confirmations) that emits `meta.no_op:true`. Kernel-managed meta
  keys (`model`, `action`, `trigger_type`, `rolled_back`) are merged on
  top of dispatcher-supplied meta and always win on collision so guests
  cannot fake them. Status codes: `200` on success, `422` when the
  dispatcher returned `Success=false` (action declined for business
  reasons), `404` on `ErrActionNotFound` / `ErrRecordNotFound` /
  `ErrModelNotFound`, `400` on `ErrInvalidID`, `501` on
  `ErrNoActionResolver` / `ErrUnsupportedTriggerType`. Tests cover the
  noop happy path (built-in dispatcher), wasm + RunInTx commit (mutation
  visible after commit), wasm + RunInTx rollback on `Success=false`
  (mutation reverted), webhook (no tx handle threaded), dispatcher
  returning a Go error (500 bubble), action-not-found (404),
  record-not-found (404), invalid id (400), no resolver wired (501) and
  unknown trigger type (501). New errors: `ErrActionNotFound`,
  `ErrNoActionResolver`, `ErrUnsupportedTriggerType` — wired through
  `handler.handleError` so the action endpoint shares the existing CRUD
  status mapping.
- **`dynamic.Service` emits canonical CRUD events post-commit.** Every
  `Create / Update / Delete` routed through the dynamic engine now publishes
  `<addonKey>.<model>.<created|updated|deleted>` on the in-process
  `events.Bus`, with payload `*dynamic.CanonicalEvent`
  (JSON shape `{id, before?, after?}`). `created` carries `{id, after}`,
  `updated` carries `{id, before, after}` (snapshot loaded before the input
  merge), `deleted` carries `{id, before}` (best-effort snapshot — `before`
  is `nil` if the row was already gone or out of tenant scope at publish
  time). Wired via two new optional `dynamic.Config` fields:
  `Bus dynamic.Publisher` (`*events.Bus` satisfies the interface; an
  internal `Publisher` interface decouples `dynamic` from `events` to avoid
  a `dynamic → events → security → bundle → dynamic` cycle) and
  `AddonKeyForModel func(ctx, model) string` (returns the addon owner of a
  model; defaults to `"kernel"` for core models). Apps that do not wire a
  `Bus` keep the previous behaviour — the publish step is a no-op. Bus
  errors are swallowed because the DB has already committed; the bus logs
  failures itself. `BulkExport / Import` paths in
  `dynamic/handler_export.go` do **not** route through `Service.Create` and
  therefore do not emit events — subscribers that need to track imported
  rows must subscribe to the bulk handler separately. Tests cover happy-path
  fan-out (`TestEvents_FanOut`), no-bus no-op (`TestEvents_NoBusIsNoop`),
  and the default `kernel` namespace fallback
  (`TestEvents_DefaultAddonKeyKernel`).
- **`metacore_host.db_query` host import (WASM ABI v1.1, runtime/wasm).**
  Read-only SQL surface for in-process addons. Each call opens a transaction,
  issues `SET LOCAL search_path TO addon_<key>, public`, runs a single
  `SELECT` (or `WITH … SELECT`), and returns the kernel `{success, data, meta}`
  envelope to the guest. Mutations / multi-statement payloads /
  `information_schema` lookups are rejected at the host layer. Capability
  enforcement runs through `security.Enforcer.CheckCapability(addonKey,
  "db:read", "addon_<key>.*")`. Wired via two new optional `Host` setters:
  `WithDB(*gorm.DB)` and `WithEnforcer(*security.Enforcer)`. Tests cover the
  happy path, mutation rejection, multi-statement rejection, introspection
  rejection, capability denial, driver error rollback, typed args, and
  literal-quoted keywords with `sqlmock`.

### Removed

- **`flow` package — extracted to consumer (link).** The workflow DAG engine
  was domain-specific to link's conversation routing (no other host imported
  it). Cross-repo recon confirmed link was the only consumer; the engine is
  now part of `link/backend/internal/flow` (asteby-hq/link#129). Hosts that
  need a similar primitive should fork from link or implement their own.
  **Breaking change** — consumers upgrading past this version must remove
  any import of `github.com/asteby/metacore-kernel/flow`.

---

## [0.2.0] - 2026-04-24

Major feature release. Consolidates the second wave of ecosystem migration:
anything two or more apps were reimplementing is now owned by the kernel.

### feat(dynamic): Options + Search

- `Service.Options(ctx, user, OptionsQuery) *OptionsResult` and
  `Service.Search(ctx, user, SearchQuery) []Option` replace the
  hand-rolled handlers every app kept in `backend/handlers/options.go`
  and `backend/handlers/search.go`.
- Configurable via `OptionsConfigResolver`, `SearchConfigResolver`,
  `ModelResolver` and `SearchMatchClause` hooks. Default matcher is
  portable (`<col> LIKE ?`); Postgres apps override with
  `unaccent(<col>) ILIKE unaccent(?)`.
- `Handler.MountOptions(r)` exposes `/options/:model` and `/search/:model`
  with the same response envelope legacy handlers returned.
- Option projection DTO (id/value/label/name/description/image/color/icon)
  unified so `DynamicSelect` frontend components consume one shape.
- Model lookup falls back to `gorm.Statement.Parse` when a model does
  not override `TableName()` — no more forcing every gorm model to
  implement `modelbase.ModelDefiner`.

### feat(modelbase): SearchConfig / OptionsConfig types

- `SearchConfig`, `OptionsConfig`, `FieldOptionsConfig` and
  `StaticOption` now live in `modelbase` alongside `TableMetadata`,
  `ModalMetadata`, `FieldDef`, `ActionDef` and friends. Apps alias them
  the same way — `type SearchConfig = modelbase.SearchConfig` — and
  drop their parallel struct definitions.
- Re-exported from `kernel/dynamic` for service callers that prefer the
  behavioural package path.

### feat(httpx): framework-agnostic context extraction

- New `kernel/httpx` package with `ContextLookup` interface (single
  `Locals(key string) any` method) + `ExtractOrgID`, `ExtractUserID`
  and a reflection-based `GetFieldValue`. Apps plug their framework via
  a minimal adapter (`fiberLookup{c}.Locals`) and drop ~60 LOC of
  duplicated extraction helpers.

### feat(push): BroadcastToOrg + OnExpiredEndpoint hook

- `Service.BroadcastToOrg(ctx, tenantID, TenantResolver, Payload)`
  drives concurrent fanout when the resolver returns org-scoped
  subscriptions; apps stop reimplementing WaitGroup loops.
- `Config.OnExpiredEndpoint` hook fires when the provider returns
  404/410, replacing the legacy per-app post-Send `isExpiredEndpoint`
  check that never actually fired (status was returned separately from
  the error). Soft-delete semantics are now the app's choice.
- `IsExpiredStatus(status int) bool` exported helper.

### feat(strings): TitleCase helper

- New `kernel/strings` package with `TitleCase`, replacing a 96-LOC
  `utils/helpers.go` that was byte-for-byte identical across multiple
  host applications.

### feat(migrations): AutoMigrate + toposort + reset

- `AutoMigrate(db, models)` two-pass FK-safe migration,
  `AutoMigrateSorted(db, map)` with topological sort by `foreignKey:`
  gorm tags, `TopoSort(map) []any` exposed for inspection, and
  `ResetDatabase(db)` DESTRUCTIVE drop-all (Postgres CASCADE / SQLite
  per-table). All library-only, CLI-invoked from the app — never at
  boot time.
- Apps' `cmd/seed/main.go` shrinks by ~60% after adoption.

### docs(architecture): Law 0

- Codifies the criterion for kernel vs SDK vs app: "would a fresh
  e-commerce/CRM/SaaS app need this on day one?" Yes → kernel.
  "Nice to have" → SDK. "Only this app" → app.

---

## [0.1.0] - 2026-04-17

### feat(migrations): configurable Dialect field

- `Runner` now has a `Dialect goose.Dialect` field. Defaults to
  `goose.DialectSQLite3` when zero-value for full backward compatibility.
- Consumers can set `Runner{Dialect: goose.DialectPostgres}` without any
  other change.
- New unit test `TestRunnerDialect_SQLite3Explicit` covers explicit dialect.
- New integration test `TestRunnerDialect_Postgres` (build tag `integration`,
  skipped unless `TEST_POSTGRES_DSN` is set) covers a real Postgres round-trip.

### feat(log): net/http HTTPMiddleware

- Added `log.HTTPMiddleware(logger *slog.Logger) func(http.Handler) http.Handler`
  for chi / net/http consumers. Mirrors FiberMiddleware behaviour:
  reads/generates `X-Request-ID`, injects child logger via `WithLogger`, logs
  method/path/status/duration/request_id after each request.
- Package docstring updated to note Fiber and net/http middlewares coexist.
- New unit tests in `log/http_middleware_test.go`.

### feat(metrics): net/http HTTPMiddleware

- Added `metrics.HTTPMiddleware(reg *Registry) func(http.Handler) http.Handler`
  for net/http consumers. Increments `http_requests_total` and observes
  `http_request_duration_seconds` exactly like FiberMiddleware.
- Package docstring updated.
- New unit tests in `metrics/http_middleware_test.go`.

### feat(auth): extensible JWT claims (Option B)

- Added `GenerateTokenWithClaims(claims jwt.Claims, secret []byte, ttl time.Duration) (string, error)`
  and `ValidateTokenWithClaims(token string, secret []byte, claims jwt.Claims) error`
  for domain-specific claim structs (e.g. marketplace Plan/Features).
- Default `Claims` struct and `GenerateToken`/`ValidateToken` are unchanged —
  zero breaking changes.
- Package docstring documents the custom-claims pattern.
- New tests: roundtrip, empty secret, wrong secret, missing token with custom claims.

### feat(migrations): versioned runner replacing AutoMigrate

- New `migrations/` package with `Runner` struct exposing `Up`, `UpTo`, `Down`,
  and `Status` methods backed by `pressly/goose v3` with an embedded `fs.FS`
  source — migration SQL is compiled into the binary.
- `migrations.Migration` struct for version/name/applied introspection.
- Initial SQL migrations for all kernel-owned tables: `users`, `organizations`,
  `webhooks`, `webhook_deliveries`, `push_subscriptions`,
  `metacore_installations` (files under `migrations/sqlfiles/`).
- `AppConfig.RunMigrations bool` in `host`: when `true`, `NewApp` calls
  `Runner.Up` instead of GORM `AutoMigrate`; the old path is retained as a
  deprecated fallback for backward compatibility.
- New dependency: `github.com/pressly/goose/v3 v3.27.0`.

### feat(log): structured slog logger + Fiber middleware

- New `log/` package: `log.New(opts)` returns `*slog.Logger` with JSON (production)
  or text (dev) handler selected via `log.Format`; `log.Default()` convenience
  constructor for zero-config production use.
- `log.WithLogger(ctx, l)` / `log.FromContext(ctx)` propagate the request-scoped
  logger through `context.Context`; falls back to `slog.Default()` if absent.
- `log.WithRequestID`, `log.WithUserID`, `log.WithOrgID` attach standard attrs to
  a child logger.
- `log.FiberMiddleware(logger)` Fiber handler: generates/reads `X-Request-ID`,
  injects child logger into `c.Locals("logger")` and `c.UserContext()`, and logs
  every request with method, path, status, duration, and request_id on completion.
- `log.FromFiberCtx(c, fallback)` retrieves the injected logger from Fiber context.
- `AppConfig.Logger *slog.Logger` added to `host.AppConfig`; auto-initialized to
  `log.Default()` if nil.

### feat(metrics): Prometheus registry + /metrics endpoint

- New `metrics/` package: `metrics.NewRegistry()` returns a `*Registry` with a
  private `prometheus.Registry` and pre-registered metrics:
  `http_requests_total` (counter, labels: method/path/status),
  `http_request_duration_seconds` (histogram, labels: method/path),
  `ws_connections` (gauge), `webhook_deliveries_total` (counter, label: status),
  `push_sends_total` (counter, label: status). Go runtime + process collectors included.
- `metrics.FiberMiddleware(reg)` increments counters and observes latency per request.
- `metrics.Handler(reg)` exposes the registry at `/metrics` in Prometheus text format.
- `AppConfig.EnableMetrics bool` added to `host.AppConfig`; when true, mounts the
  middleware and `GET /metrics` handler via `host.App.Mount()`.
- Added `github.com/prometheus/client_golang v1.23.2` to `go.mod`.

### feat(push): real AES128GCM encryption and proper VAPID JWT

- `push.Service.Send` now performs full RFC 8291 payload encryption (HKDF +
  AES-GCM) and signs the Authorization header with a proper ES256 VAPID JWT
  (RFC 8292).  When no VAPID private key is configured the method falls back to
  plain JSON delivery, preserving backwards compatibility for tests.
- New `push/crypto.go` package-private helper: `encryptPayload` implements the
  `aes128gcm` content-encoding used by all modern push services.
- `push.GenerateVAPIDKeys` migrated to `crypto/ecdh`; the public key is now
  the canonical 65-byte uncompressed P-256 point browsers expect from
  `PushManager.subscribe`.
- `push.Payload` extended with `Image`, `Actions []Action`, `Vibrate`,
  `Silent`, `Renotify` — matching the full Web Notification API surface
  required by typical host applications.
- New `push.Action` type in `models.go`.
- New unit tests: `TestGenerateVAPIDKeys`, `TestVAPIDJWT`, `TestEncryptPayload`
  (all in `push/crypto_test.go`).
- **Decision**: completed `push/` in-place — no separate `webpush/` package
  needed.  The existing package already had VAPID key-gen, subscribe/unsubscribe,
  handler and service tests; only the crypto layer was missing.

### feat(ws): hub confirmed generic; SendConditional added

- `ws.Hub` already used `MessageType string` (not a hardcoded enum), so each
  app can declare its own message-type constants without any kernel change.
  This was confirmed correct and documented in the package-level docstring.
- Added `Hub.SendConditional(userID, predicate, primary, fallback)`: delivers
  different messages to a user's connections based on a per-connection
  predicate.  This is the generic equivalent of a conversation-aware
  "smart broadcast" — the predicate receives `Client.Context` (any), which
  apps set via `Client.SetContext(v any)`.
- Added `Client.Context any` field + `SetContext` / `GetContext` helpers
  (mutex-protected) for per-connection app state.
- `Hub.SendToUsers([]uuid.UUID, msg)` is the generic equivalent of an
  org-scoped broadcast — callers query their own DB for user IDs and pass
  the slice; the hub stays ORM-free.
- `OnNotification` hook covers notification persistence (the kernel delegates
  it; hosts handle persistence inline against their own models).
- Keepalive: `client.go` ping/pong with 60 s pong-wait + (54 s) ping-period
  matches typical browser-friendly defaults.
- **Coverage verdict**: the kernel ws hub covers all routing patterns host
  applications typically require (user routing, batch/org broadcast,
  keepalive, custom message types, persistence hook, conditional routing).

### Stable packages (no API changes this cycle)

`modelbase`, `metadata`, `permission`, `dynamic`, `query`, `webhooks`,
`security`, `host`, `installer`, `lifecycle`, `navigation`, `runtime/wasm`.

---

## [v0.2.0-alpha.1] — previous release

_(see git tags for history)_
