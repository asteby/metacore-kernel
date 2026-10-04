# Audit-column standard

Every table the kernel materializes can answer **when and who** for each change,
whether or not the addon manifest declares the columns.

| column          | type                  | filled by                                  |
|-----------------|-----------------------|--------------------------------------------|
| `created_at`    | timestamptz NOT NULL DEFAULT NOW() | insert                         |
| `updated_at`    | timestamptz NOT NULL DEFAULT NOW() | every update                   |
| `deleted_at`    | timestamptz, nullable | soft delete (tombstone)                    |
| `created_by_id` | uuid, nullable        | insert (actor)                             |
| `updated_by_id` | uuid, nullable        | insert and every update (actor)            |
| `deleted_by_id` | uuid, nullable        | soft delete (actor)                        |

Indexes: `deleted_at` (and `organization_id, deleted_at` on org-scoped tables)
and `created_by_id`, as before. `updated_by_id` / `deleted_by_id` are not
indexed (rarely filtered; avoid write amplification).

## Who the actor is

* A request: the `AuthUser` of the context (`Service.Create/Update/Delete/Restore`).
* A wasm invocation (`data_mutate`, `data_batch`, and so actions/hooks that
  write through them): the invocation actor (`dynamic.ActorIDFromContext`, the
  user whose action started the chain).
* Unattended work (schedules, webhooks, connectors, any invocation with no
  actor): **`dynamic.SystemActorID`** (`00000000-0000-0000-0000-000000000001`),
  the same id `NewSystemCaller`, the canonical-event `ActorID` and the approval
  `RequestedBy` already use, so the audit columns and the universal activity log
  agree. `dynamic.ActorOrSystem(id)` is the single rule.

The values are runtime-owned: `Service.Create/Update` **discard** any audit key in
the client payload, and wasm guests get `invalid_request` if they name one.

## Opt-out

| model                      | columns                                             |
|----------------------------|-----------------------------------------------------|
| default                    | all six                                             |
| `append_only: true`        | `created_at`, `created_by_id` (a ledger row is never updated or deleted) |
| `audit: false` (v3 model)  | `created_at`, `updated_at` (historical shape) + `deleted_at` only if `soft_delete`/declared |

Use `audit: false` only for purely technical tables nobody edits through the
product (a cache, a derived projection). When in doubt keep the default.

If the manifest already declares one of the columns, **the declaration wins**
(the kernel only adds the ones that are missing). The v3 validator requires the
standard type (`timestamp`/`timestamptz` for `*_at`, `uuid` for `*_by_id`); a
clash is the `audit_column_type` rule: a warning inside the compat window
(install/upgrade), an error when publishing. `deleted_at` declared in the
manifest still maps to `ModelDefinition.SoftDelete`.

The legacy host switches (`DDLOptions.IncludeCreatedBy`, `AlwaysSoftDelete`,
`StructOptions.SoftDeleteGorm`, `IncludeCreatedBy`, `SingleSchemaDDLOptions`)
are redundant now but still accepted; they additionally force their column for
`audit: false` / `append_only` models.

## Upgrade of existing installations

`dynamic.SyncSchema` (called by `CreateTable*`, by `Install` and by `Upgrade`
**before** the SQL migrations) adds the missing standard columns with
`ALTER TABLE … ADD COLUMN IF NOT EXISTS` plus the indexes. When the host serves
the models from another schema (`MigrationSchema`, ops: `public`) the installer
also runs `dynamic.EnsureAuditColumns(db, primarySchema, def)` after the host's
`MaterializeModels`; the kernel-added timestamp type follows the table's own
`created_at` (`timestamp` vs `timestamptz`). Both run inside the Upgrade
pre-flight dry run (rolled back). Hosts that own their DDL can call
`SchemaEngine.AuditColumnsDDL(schema, def, existingColumns, opts)` to get the
statements.

Result: **a migration may assume the six columns** (e.g.
`CREATE TRIGGER … UPDATE OF deleted_at ON invoices` — the customers@037 case)
without the model declaring them.

Cost on big tables: nullable columns without default are a catalog-only change
on PostgreSQL 11+, and `created_at`/`updated_at` with the *stable* default
`NOW()` use the same fast-default path (no rewrite). The `CREATE INDEX` on
`deleted_at` / `created_by_id` is the only cost proportional to table size and
blocks writes while it builds; schedule the first upgrade of a very large table
off-peak (a `CONCURRENTLY` build cannot run inside the upgrade transaction).

## Read side

`TableMetadata.audit` (`modelbase.AuditMeta`) names, per record, the row key a UI
reads:

```json
"audit": { "created_at": "created_at", "created_by": "created_by_id",
           "updated_at": "updated_at", "updated_by": "updated_by_id",
           "deleted_at": "deleted_at", "deleted_by": "deleted_by_id" }
```

A key is omitted when the model lacks that column (an append-only ledger has no
`updated_*`/`deleted_*`). `*_by` values are user ids (`SystemActorID` for
unattended work); the SDK resolves them to names. `metadata.Service` derives the
block from the model's fields (`modelbase.DeriveAuditMeta`); a host that builds
metadata from the manifest can use `dynamic.AuditMetaFor(def, …)`.

## Soft delete semantics

The runtime struct now always carries `deleted_at` as `gorm.DeletedAt`, so GORM
filters tombstoned rows on reads and `Service.Delete` soft-deletes (stamping
`deleted_by_id`). `Service.Restore` clears the tombstone and records the
restorer as `updated_by_id`. Caveat: unique indexes declared on a column are not
partial, so a soft-deleted row still holds its unique value; declare a `unique`
rule (which is partial on `deleted_at IS NULL`) when a deleted value must be
reusable.

## What changes for whom

* **Hosts (ops/hub/pitsline):** nothing mandatory for the DDL; the redundant
  options can be dropped. If a host builds the runtime struct with
  `ToReflectType`/`BuildStructType` over tables it created itself, run
  `EnsureAuditColumns` for each model at boot **before** serving writes (the
  struct now includes `updated_by_id`/`deleted_by_id`; inserting into a table
  without them fails with 42703). Compiled models embedding
  `modelbase.BaseUUIDModel` are unchanged (no `updated_by_id`/`deleted_by_id`
  fields yet — add them with a host migration when adopting).
* **Addons:** no need to declare these columns; migrations can assume them.
  Do not write them from handlers — they are host-stamped.
