# Kernel gaps that repeat across the 7 Leguas migration analyses

Status: design only. None of this is implemented in `feat/native-parity`; each
item is a separate PR. Priority is by how often the four analyses
(`docs/migracion-go/01..04`) hit the gap. Everything stays generic and
multi-industry; nothing here names a vertical.

## 1. Formula engine: functions and exact decimals

Today `manifest/computeexpr` evaluates `+ - * /` over float64. Money must never
be float.

- Add pure functions: `round(x, places)`, `ceil(x)`, `ceil_to(x, multiple)`,
  `floor(x)`, `min(a, b, ...)`, `max(a, b, ...)`, `coalesce(a, b, ...)`, `abs(x)`.
  Allowlisted in the parser (no identifiers beyond columns, no user functions).
- Evaluate on an exact decimal type (shopspring-style `Decimal`, or `math/big`
  rationals with a declared scale) instead of float64. A target column of type
  `numeric(p,s)` fixes the result scale and rounding mode (half away from zero,
  as `money/` already does); integer targets keep today's rounding. The evaluator
  returns a decimal string for numeric columns so the JSON never round-trips
  through float. `money/` already has the minor-unit primitives: reuse them, do
  not add a second decimal implementation.
- Back-compat: expressions that parse today keep their value for inputs that
  float64 represented exactly; differences appear only where float was wrong
  (documented in the changelog as a fix, not a break).
- Tests: table-driven over the functions, decimal edge cases (0.1+0.2, 19.99*100,
  half-way rounding, negative), and the existing Tier-2 suite unchanged.

## 2. Per-line tax parameterisation

A tax is data, not a constant. Add a `tax` primitive (not a formula hack):

- `models[].tax_lines`: `{rate_field | catalog, withholding_field?, inclusive?}`
  naming where a line's rate/withholding comes from (a column of the line, or a
  catalog model keyed by tax code), plus the total columns to fill.
- Computed through the exact-decimal engine of item 1 (so it depends on it).
- No default rate anywhere in the kernel.

## 3. Generic ledger / balance primitive

A cell (composite key) with a signed delta and a configurable constraint.

- Manifest: `models[].ledger {cell: [cols], delta: col, floor: 0|null, movement_model}`.
  Writing a movement row (append-only, reuses `append_only`) applies the delta to
  the balance row in the SAME transaction under a row lock (item 4), refuses a
  result below `floor` with a stable code, publishes the event after commit
  (reuses emit-on-commit) and is idempotent by key (item 5).
- Built from existing primitives: `append_only`, `Locking: row`, the commit hook,
  `idempotency`. The new part is the declarative cell/delta/floor contract and
  one tested implementation, so stock, loyalty points, wallet balance, quotas and
  cash drawers do not each re-implement it.

## 4. Row lock / FOR UPDATE for handlers

`Model.locking: row` already locks around kernel-driven guards. Handlers (wasm
and compiled) need the same on demand: a host import / `NativeCall.LockRows(model,
ids)` that issues `SELECT ... FOR UPDATE` inside the invocation's transaction
(`InvokeForInTx`), tenant-scoped through the same `execute*` seam, ordered by id
to avoid deadlocks, with a `lock_timeout`. Only meaningful with a tx: without
one it returns `no_transaction`.

## 5. Global idempotency key for actions and writes

`idempotency/` exists (middleware + store) and `Action.idempotency` covers
`ExecAction`. Gap: writes through the generic CRUD and wasm/compiled
`data_mutate`. Extend the same store with an `Idempotency-Key` header/`_idem` arg
on create/update, scoped `(org, model, op, key)`, replaying the first response.
One store, one key shape; actions keep their declarative `key_field`.

## 6. Partial unique index in the v3 schema

Already supported: `models[].indices[].where` with `unique: true` emits a
partial `CREATE UNIQUE INDEX ... WHERE` (see `manifest.ParseIndexWhere`). The
migration analyses asked for it because it is under-documented: add an example
and a `docs/spec/v3/README.md` section, and extend `Where` beyond
`IS [NOT] NULL` / `<col> <op> <literal>` only if a concrete case needs it.

## 7. Per-org settings with encrypted secrets and "test connection"

Largely covered by `connectors[]` (per-org credentials stored encrypted with the
installer `MasterKey`, `test_export`, `connector_connected` conditions). The gap
is plain addon `settings[]` of `type: secret` (encrypted at rest, never served to
the client, write-only in the UI) and a `settings[].test` action reference. Reuse
the connector credential storage rather than a second one; a setting of type
secret is a connector credential of an implicit connector.

## Order of work

1 (and 2 on top of it) first: highest frequency and the only one that changes an
existing behaviour. Then 5 and 4 (small, unblock 3), then 3. 6 is documentation.
7 after confirming the connector reuse. Each is its own PR with a changeset
entry; none belongs in this branch.
