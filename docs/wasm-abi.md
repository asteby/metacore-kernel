# WASM ABI (v1.3 — proposal, kernel-side)

The metacore kernel can run addon backends as sandboxed WebAssembly modules
via [wazero](https://wazero.io). This document is the kernel-side contract
between the guest (the addon) and the host (this kernel). The addon-author
view lives in [`metacore-sdk/docs/wasm-abi.md`](https://github.com/asteby/metacore-sdk/blob/main/docs/wasm-abi.md);
keep them in sync.

> ABI version: **1.3** (proposal — `event_emit` host import added on top of
> v1.2; guests built against 1.0 / 1.1 / 1.2 keep working — purely additive).
> Bundled via `manifest.backend.runtime = "wasm"`.
> Implementation: `runtime/wasm/abi.go`, `runtime/wasm/capabilities.go`.

### Version history

| Version | Status   | Changes |
|---------|----------|---------|
| 1.0     | shipped  | initial surface: `log`, `env_get`, `http_fetch`. |
| 1.1     | proposal | adds `db_query` host import; guests built against 1.0 keep working. |
| 1.2     | proposal | adds `db_exec` host import (writes, scoped to `addon_<key>.*`, auto-rollback on error); requires the action handler to invoke the guest with `Host.InvokeInTx` so the guest's writes piggy-back on the action's `*gorm.DB` transaction. |
| 1.3     | proposal | adds `event_emit` host import; guests publish a `<name>, <payload>` pair through the kernel's in-process `events.Bus`. Capability gated by `event:emit <name>` and tenant-scoped by the per-invocation `orgID` the host carries on the context bag. |

## 1. Declaration

```json
"backend": {
  "runtime": "wasm",
  "entry": "backend/backend.wasm",
  "exports": ["resolve_ticket", "ping"],
  "memory_limit_mb": 64,
  "timeout_ms": 10000
}
```

Only symbols listed in `exports` can be dispatched by the host. Limits
default to 64 MiB and 10 s.

## 2. Required guest exports

Every WASM module MUST export:

### `memory`

The module's linear memory (default name `memory`). The host reads and
writes buffers through it.

### `alloc(size: i32) -> i32`

A bump (or pool) allocator the host calls to reserve `size` bytes in guest
memory before copying the request payload in. Return value is the guest
pointer. Must succeed for any size up to the configured memory limit.

### `<action_key>(ptr: i32, len: i32) -> i64`

One per entry in `exports`. `(ptr, len)` is the request body (JSON, by
convention). The return value is a **packed (ptr, len)** response:

```
result_i64 = (uint64(ptr) << 32) | uint64(len)
```

A return of `0` means "empty success". To signal an error, the guest writes
a JSON envelope of the form `{"error": "..."}` and the host-side surface
layer interprets it. Exceeding `timeout_ms` aborts the instance.

## 3. Host imports (module `metacore_host`)

The host module exposes these functions; all pointer arguments are i32 and
reference guest memory:

```
log(msgPtr i32, msgLen i32)
  -> void. Writes a structured log line tagged with the addon key.

env_get(keyPtr i32, keyLen i32) -> i64
  -> packed (ptr, len) in guest memory of the setting value, or 0 if missing.
     Backed by the installation's `settings` map; secrets are allowed.

http_fetch(urlPtr, urlLen, methPtr, methLen, bodyPtr, bodyLen i32) -> i64
  -> packed (ptr, len) of the response body. Subject to the addon's
     `http:fetch` capabilities and the egress SSRF guard (see capabilities.md).

db_query(sqlPtr i32, sqlLen i32, argsPtr i32, argsLen i32) -> i64   [v1.1]
  -> packed (ptr, len) of a JSON envelope with rows. Scoped to the addon's
     own schema (`SET LOCAL search_path TO addon_<key>, public` per call) and
     gated by `db:read` capabilities for any cross-schema reference. Read-only
     in v1.1 — see § 9 for the full contract.

db_exec(sqlPtr i32, sqlLen i32, argsPtr i32, argsLen i32) -> i64    [v1.2]
  -> packed (ptr, len) of a JSON envelope reporting `rowsAffected` and the
     `lastInsertId` (when applicable). Mutating SQL only (`INSERT`, `UPDATE`,
     `DELETE`, `MERGE`). Scoped to `addon_<key>.*` and gated by the implicit
     `db:write addon_<key>.*` capability — cross-schema writes require an
     explicit `db:write <schema>.<table>` grant. **Requires** the host to have
     entered `Host.InvokeInTx`: the call piggy-backs on the action handler's
     `*gorm.DB` transaction so a non-zero error from the guest (or any
     panic / timeout) auto-rolls back the entire action. See § 10 for the
     full contract.

event_emit(eventPtr i32, eventLen i32, payloadPtr i32, payloadLen i32) -> i64 [v1.3]
  -> packed (ptr, len) of a JSON envelope reporting how many subscribers the
     payload reached. Publishes `<event>` + `<payload>` through the kernel's
     in-process `events.Bus`. Gated by the addon's `event:emit <name>`
     capability (wildcards `prefix.*` honoured) and tenant-scoped by the
     per-invocation `orgID` the host stashed on the context bag. Delivery is
     synchronous and best-effort: subscriber errors are logged but do not
     surface to the publisher. See § 12 for the full contract.
```

The host allocates response buffers inside guest memory via `alloc`, writes
into them, and returns the packed pointer. The guest is responsible for
reading before triggering another allocation.

## 4. Minimal TinyGo example

```go
// backend/main.go — stub que recibe payload y devuelve eco.
package main

import (
	"encoding/json"
	"unsafe"
)

//go:wasmimport metacore_host log
func hostLog(ptr, length uint32)

// alloc es el bump allocator que el host llama antes de escribir el payload.
//
//go:export alloc
func alloc(size uint32) uint32 {
	buf := make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

// ping recibe (ptr, len) y devuelve un i64 packeado (ptr<<32)|len.
//
//go:export ping
func ping(ptr, length uint32) uint64 {
	in := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
	var req struct{ Message string `json:"message"` }
	_ = json.Unmarshal(in, &req)

	msg := []byte("hello from wasm: " + req.Message)
	hostLog(uint32(uintptr(unsafe.Pointer(&msg[0]))), uint32(len(msg)))

	resp, _ := json.Marshal(map[string]string{"reply": "pong", "echo": req.Message})
	p := uint32(uintptr(unsafe.Pointer(&resp[0])))
	return (uint64(p) << 32) | uint64(len(resp))
}

func main() {} // requerido por tinygo
```

## 5. Building

### With TinyGo directly

```bash
tinygo build -target=wasi -opt=z -no-debug -o backend/backend.wasm ./backend/
```

Flags explained:

- `-target=wasi` — enables WASI stdlib shims needed for `encoding/json`.
- `-opt=z` — optimize for size. Typical backends end up 100-400 KiB.
- `-no-debug` — drops DWARF sections; the host does not need them.

### With the CLI wrapper

```bash
metacore compile-wasm .
```

Equivalent to the command above, but with the correct flags and output path
derived from `manifest.backend.entry`.

## 6. Memory & reentrancy rules

- Each invocation runs in a **fresh module instance**. Globals do not
  persist between calls.
- The guest allocator may be a single-shot bump allocator; the host
  tolerates that since each call gets a new instance.
- Callbacks into host imports are synchronous. The host serializes
  invocations per installation.

## 7. Error surface

Return packed pointer to a JSON object. The recommended shape is:

```json
{ "error": { "code": "not_found", "message": "ticket 42 missing" } }
```

The host forwards this verbatim to the caller (webhook response, action
result, tool invocation). Panics and abort traps are reported as
`{"code": "runtime_error"}`.

## 8. Capability enforcement

Host imports check the addon's compiled capabilities before execution:

- `http_fetch` calls `Capabilities.CanFetch(url)`.
- `db_query` (v1.1) parses the SQL, walks every referenced relation, and
  calls `Capabilities.CanReadModel(<schema>.<table>)` for any reference
  that resolves outside `addon_<key>`. The owning addon's own schema is
  always permitted (implicit `addon_<key>.*`).
- `db_exec` (v1.2) parses the SQL, asserts the top-level statement is a
  mutation (`INSERT` / `UPDATE` / `DELETE` / `MERGE`), and walks every target
  relation. `Capabilities.CanWriteModel(<schema>.<table>)` is called for any
  target outside `addon_<key>`; the addon's own schema is always permitted
  via the implicit `db:write addon_<key>.*` capability. Pure subquery
  references in `WHERE` / `RETURNING` go through the `db_query` read path
  (`CanReadModel`).
- `event_emit` (v1.3) does **not** re-implement the capability scan inline:
  it forwards directly to `events.Bus.Publish(ctx, addonKey, event, orgID,
  payload)`, which already gates publication through the same
  `security.Enforcer.CheckCapability(addonKey, "event:emit", event)` path —
  i.e. `Capabilities.CanEmit(event)`. The host's only added enforcement is
  validating the event name (§ 12.2) and ensuring an `orgID` is bound to the
  invocation (§ 12.6).

If an import is denied, the host returns a packed buffer whose JSON payload
contains `{"error":{"code":"forbidden","message":"..."}}`.

## 9. `db_query` — scoped read-only SQL (v1.1)

`db_query` is the dedicated database import. It is intentionally narrow: a
single read-only statement, scoped to the addon's schema, parameterised, and
capability-checked. Mutating SQL belongs to a separate `db_exec` import that
will land in a future minor version.

### 9.1 Signature

```
db_query(sqlPtr i32, sqlLen i32, argsPtr i32, argsLen i32) -> i64
```

| Param      | Type | Meaning                                                                |
|------------|------|------------------------------------------------------------------------|
| `sqlPtr`   | i32  | Guest pointer to the SQL text.                                         |
| `sqlLen`   | i32  | Length in bytes (UTF-8). Hard cap: 16 KiB.                             |
| `argsPtr`  | i32  | Guest pointer to a JSON array of positional arguments. May be `0`.     |
| `argsLen`  | i32  | Length of the JSON array buffer. `0` if the query has no parameters.   |
| **return** | i64  | Packed `(ptr<<32)\|len` of the response envelope (see § 9.4).          |

A return of `0` is reserved and currently never produced — `db_query` always
allocates an envelope, even for zero-row results.

### 9.2 SQL contract

- **Read-only**: only `SELECT` (and `WITH … SELECT`) is accepted in v1.1.
  Any other top-level statement (`INSERT`, `UPDATE`, `DELETE`, `MERGE`,
  `CREATE`, `DROP`, `ALTER`, `TRUNCATE`, `COPY`, `GRANT`, `SET`, `CALL`,
  `DO`, `LISTEN`, `NOTIFY`, `BEGIN`, `COMMIT`) is rejected with
  `invalid_sql`.
- **Single statement**: the input is parsed into a statement list and must
  contain exactly one node. Trailing `;` is tolerated; multi-statement
  payloads are rejected with `invalid_sql`.
- **Parameters**: positional placeholders use Postgres syntax (`$1`, `$2`,
  …). The arg count must equal the highest placeholder index — otherwise
  `arg_count_mismatch`.
- **No `SET search_path`**: the host issues `SET LOCAL search_path` on
  every call and rejects guest-side overrides at parse time.
- **No `pg_*` / `information_schema`** lookups in v1.1 — these are
  filtered to keep the surface explainable. (Schema introspection has its
  own dedicated import on the roadmap.)

### 9.3 Schema scope & capability check

The host wraps every invocation in a transaction-scoped `SET LOCAL
search_path TO addon_<key>, public`. Bare table names therefore resolve
against the addon's own schema first.

For each parsed relation reference the host computes a fully-qualified
`<schema>.<table>` and decides:

| Reference                         | Outcome                                                                  |
|-----------------------------------|--------------------------------------------------------------------------|
| Bare name resolved into `addon_<key>` | Allowed. Implicit `addon_<key>.*` capability.                        |
| `addon_<key>.<table>` (qualified) | Allowed.                                                                 |
| `public.<table>` or other schema  | Requires `db:read <schema>.<table>` or `db:read <schema>.*`.             |
| `pg_*` / `information_schema.*`   | Always denied (`forbidden`, `reason: "introspection_disabled"`).         |

Cross-tenant scoping (org filters) is **orthogonal** and applied by the
host transparently for any model that carries an `org_id` column — see
[`permissions.md`](./permissions.md) for the row-level rules.

### 9.4 Response envelope

The response follows the kernel `{success, data, meta}` convention:

```json
{
  "success": true,
  "data": {
    "rows":    [ { "id": 1, "title": "..." }, … ],
    "rowCount": 42,
    "columns": [
      { "name": "id",    "type": "int8" },
      { "name": "title", "type": "text" }
    ]
  },
  "meta": {
    "schema":     "addon_tickets",
    "durationMs": 7,
    "truncated":  false
  }
}
```

Errors share the same outer shape:

```json
{
  "success": false,
  "error":   { "code": "forbidden", "message": "addon \"tickets\" lacks db:read \"billing.invoices\"" },
  "meta":    { "schema": "addon_tickets", "durationMs": 1 }
}
```

Defined error codes:

| Code                  | When                                                                |
|-----------------------|---------------------------------------------------------------------|
| `invalid_sql`         | Parse failure, multi-statement, non-`SELECT`, banned construct.     |
| `arg_count_mismatch`  | Highest `$N` placeholder ≠ JSON args length.                        |
| `arg_decode`          | `argsPtr/argsLen` is not valid JSON or contains an unsupported type.|
| `forbidden`           | Capability check failed for one of the referenced relations.        |
| `query_timeout`       | Statement exceeded the per-call DB deadline (default 5 s, see § 9.5).|
| `row_limit_exceeded`  | Result set exceeded the configured row cap (default 10 000).        |
| `db_error`            | Underlying driver/SQL error (message redacted, code preserved).     |

### 9.5 Limits

| Knob                | Default | Configurable via                                  |
|---------------------|---------|---------------------------------------------------|
| Max SQL length      | 16 KiB  | host-side (`runtime/wasm` config).                |
| Max args            | 64      | host-side.                                        |
| Per-call deadline   | 5 s     | bounded by `manifest.backend.timeout_ms` (lower wins). |
| Max rows            | 10 000  | host-side; emits `row_limit_exceeded` past it.    |
| Max response bytes  | 8 MiB   | host-side; mirrors the `http_fetch` cap.          |

### 9.6 Allowed argument types

JSON args are decoded into the driver's native types as follows:

| JSON                      | Postgres parameter type    |
|---------------------------|----------------------------|
| `null`                    | `NULL`                     |
| `true` / `false`          | `bool`                     |
| integer literal           | `int8`                     |
| floating literal          | `float8`                   |
| string                    | `text`                     |
| `{"$bytes": "<base64>"}`  | `bytea`                    |
| `{"$uuid":  "<uuid>"}`    | `uuid`                     |
| `{"$ts":    "<RFC3339>"}` | `timestamptz`              |

Plain JSON arrays/objects are rejected with `arg_decode` — the driver-level
`jsonb` round-trip is intentionally explicit (`{"$jsonb": …}` is reserved
for v1.2 once nested encoding is finalised).

### 9.7 Out of scope for v1.1

These are deliberately **not** in v1.1 and will land as separate proposals
once the read path is exercised in production:

- `db_exec` for `INSERT`/`UPDATE`/`DELETE` — **lifted in v1.2**, see § 10.
- Streaming cursors. v1.1 buffers the full result set in host memory; large
  reports should pre-aggregate in SQL.
- Prepared-statement caching across invocations. Each call re-prepares.
- Schema introspection (`information_schema`). A dedicated import will
  expose a curated subset.

## 10. `db_exec` — addon-scoped writes (v1.2)

`db_exec` is the dedicated mutation import. It is intentionally narrow: a
single mutating statement, scoped to the addon's own schema, parameterised,
capability-checked, and **always executed against the action handler's open
`*gorm.DB` transaction** so an error inside the guest rolls back the whole
action atomically (manifest record write + side-effects + audit row).

> Pre-condition: the host must have entered `Host.InvokeInTx(ctx, tx, ...)`.
> Calling `db_exec` from a guest invoked by the plain `Host.Invoke` (no tx
> in flight) returns `{success:false, error:{code:"no_active_tx"}}` — see
> § 10.6. Action triggers wired with `Trigger.Type="wasm"` + `RunInTx=true`
> always satisfy this pre-condition (see audit
> [`docs/audits/2026-05-04-action-trigger-gap.md`](audits/2026-05-04-action-trigger-gap.md)).

### 10.1 Signature

```
db_exec(sqlPtr i32, sqlLen i32, argsPtr i32, argsLen i32) -> i64
```

| Param      | Type | Meaning                                                                |
|------------|------|------------------------------------------------------------------------|
| `sqlPtr`   | i32  | Guest pointer to the SQL text.                                         |
| `sqlLen`   | i32  | Length in bytes (UTF-8). Hard cap: 16 KiB.                             |
| `argsPtr`  | i32  | Guest pointer to a JSON array of positional arguments. May be `0`.     |
| `argsLen`  | i32  | Length of the JSON array buffer. `0` if the statement has no parameters.|
| **return** | i64  | Packed `(ptr<<32)\|len` of the response envelope (see § 10.4).         |

A return of `0` is reserved and currently never produced — `db_exec` always
allocates an envelope, even for zero-row mutations.

### 10.2 SQL contract

- **Mutating only**: only `INSERT`, `UPDATE`, `DELETE` and `MERGE` (and a
  leading `WITH … <mutation>` whose final statement is one of those four)
  are accepted. Any other top-level statement (`SELECT`, `CREATE`, `DROP`,
  `ALTER`, `TRUNCATE`, `COPY`, `GRANT`, `SET`, `CALL`, `DO`, `LISTEN`,
  `NOTIFY`, `BEGIN`, `COMMIT`, `ROLLBACK`, `SAVEPOINT`) is rejected with
  `invalid_sql`. Read-only queries belong to `db_query`.
- **Single statement**: parsed into a statement list and must contain
  exactly one top-level node. Trailing `;` is tolerated; multi-statement
  payloads are rejected with `invalid_sql`.
- **Transaction control is forbidden**: `BEGIN`, `COMMIT`, `ROLLBACK`,
  `SAVEPOINT`, `RELEASE` and `SET TRANSACTION` are rejected at parse time.
  The host owns the transaction; the guest only contributes statements to
  it.
- **Parameters**: positional placeholders use Postgres syntax (`$1`, `$2`,
  …). The arg count must equal the highest placeholder index — otherwise
  `arg_count_mismatch`. JSON arg encoding matches § 9.6 verbatim.
- **No `SET search_path`**: the host issues `SET LOCAL search_path` on
  every call and rejects guest-side overrides at parse time.
- **No `pg_*` / `information_schema` writes** — always denied
  (`forbidden`, `reason: "introspection_disabled"`).
- **`RETURNING` is allowed.** The returned rows are projected into
  `data.returning` (same shape as `db_query.data.rows`). The row cap from
  § 9.5 applies; exceeding it triggers `row_limit_exceeded` and rolls back
  the action (see § 10.7).

### 10.3 Schema scope & capability check

The host issues `SET LOCAL search_path TO addon_<key>, public` once when it
opens the action transaction, so bare table names resolve against the
addon's own schema first (mirrors `db_query`).

For each parsed **target** relation reference (the relation being mutated)
the host computes a fully-qualified `<schema>.<table>` and decides:

| Reference                              | Outcome                                                                              |
|----------------------------------------|--------------------------------------------------------------------------------------|
| Bare name resolved into `addon_<key>`  | Allowed. Implicit `db:write addon_<key>.*` capability.                               |
| `addon_<key>.<table>` (qualified)      | Allowed.                                                                             |
| `public.<table>` or other schema       | Requires explicit `db:write <schema>.<table>` or `db:write <schema>.*`.              |
| `pg_*` / `information_schema.*`        | Always denied (`forbidden`, `reason: "introspection_disabled"`).                     |

`USING` / `FROM` source relations in `UPDATE` / `DELETE` and subqueries in
`WHERE` / `RETURNING` go through the **read** capability check
(`db:read`), since the guest is only inspecting them — same rules as
`db_query` (§ 9.3).

Cross-tenant scoping (`org_id` row-level filters) is **orthogonal** and
applied transparently by the host for any model that carries an `org_id`
column — see [`permissions.md`](./permissions.md). For mutations the host
also rejects `UPDATE` / `DELETE` whose `WHERE` clause does not constrain
`org_id` to the principal's org (`forbidden`, `reason: "missing_org_filter"`).

### 10.4 Response envelope

Success follows the kernel `{success, data, meta}` convention:

```json
{
  "success": true,
  "data": {
    "rowsAffected": 3,
    "lastInsertId": "8b1f…",
    "returning": [
      { "id": "8b1f…", "status": "refunded", "amount_cents": 4200 }
    ],
    "columns": [
      { "name": "id",           "type": "uuid" },
      { "name": "status",       "type": "text" },
      { "name": "amount_cents", "type": "int8" }
    ]
  },
  "meta": {
    "schema":     "addon_refunds",
    "durationMs": 11,
    "txId":       "tx_01HXY…",
    "truncated":  false
  }
}
```

- `lastInsertId` is populated when the statement is a single-row `INSERT`
  whose target has a primary key the host can recover (either via
  `RETURNING` requested by the host, or via the driver's `LastInsertId()`
  for integer PKs). Multi-row inserts leave it `null`; rely on `returning`
  instead.
- `returning` / `columns` are omitted when the statement does not include a
  `RETURNING` clause.
- `meta.txId` is the kernel-assigned id of the action transaction, present
  so audit trails can correlate guest writes with the host's commit/rollback
  decision.

Errors share the same outer shape and **always** roll back the action (see
§ 10.7):

```json
{
  "success": false,
  "error":   { "code": "forbidden", "message": "addon \"refunds\" lacks db:write \"billing.invoices\"" },
  "meta":    { "schema": "addon_refunds", "durationMs": 1, "txId": "tx_01HXY…" }
}
```

Defined error codes (in addition to those shared with `db_query`):

| Code                  | When                                                                  |
|-----------------------|-----------------------------------------------------------------------|
| `invalid_sql`         | Parse failure, multi-statement, non-mutation, banned tx-control verb. |
| `arg_count_mismatch`  | Highest `$N` placeholder ≠ JSON args length.                          |
| `arg_decode`          | `argsPtr/argsLen` is not valid JSON or contains an unsupported type.  |
| `forbidden`           | Capability check failed for one of the target / source relations.     |
| `missing_org_filter`  | `UPDATE` / `DELETE` did not constrain `org_id` to the principal's org.|
| `query_timeout`       | Statement exceeded the per-call DB deadline (default 5 s, see § 10.5).|
| `row_limit_exceeded`  | `RETURNING` produced more rows than the configured cap (default 10 000).|
| `constraint_violation`| Underlying SQLSTATE 23xxx (FK / unique / check). Driver detail redacted; SQLSTATE preserved in `error.sqlstate`. |
| `serialization_failure`| Underlying SQLSTATE 40001 / 40P01 (concurrent-update conflict). Caller is expected to retry the whole action. |
| `db_error`            | Any other driver/SQL error (message redacted, SQLSTATE preserved).    |
| `no_active_tx`        | Guest invoked via `Host.Invoke` (no transaction in flight).           |

### 10.5 Limits

| Knob                | Default | Configurable via                                       |
|---------------------|---------|--------------------------------------------------------|
| Max SQL length      | 16 KiB  | host-side (`runtime/wasm` config). Mirrors `db_query`. |
| Max args            | 64      | host-side. Mirrors `db_query`.                         |
| Per-call deadline   | 5 s     | bounded by `manifest.backend.timeout_ms` (lower wins). |
| Max `db_exec` calls per invocation | 32 | host-side. Caps blast radius of a runaway guest. |
| Max `RETURNING` rows | 10 000 | host-side; emits `row_limit_exceeded` past it.         |
| Max response bytes  | 8 MiB   | host-side; mirrors `db_query` and `http_fetch`.        |

The per-call deadline is enforced by `SET LOCAL statement_timeout` inside
the action transaction. The action-level deadline (`Trigger.TimeoutMs` or
`BackendSpec.TimeoutMs`) still bounds the total wall time of the guest;
the lower of the two wins.

### 10.6 Transaction reuse — the `*gorm.DB` contract

`db_exec` is the only host import that is **not** safe to call from a
guest invoked via the plain `Host.Invoke`. The host enforces this at the
call site:

```go
// runtime/wasm/wasm.go (sketch — implementation lives in v1.2 PR)
func (h *Host) InvokeInTx(
    ctx context.Context,
    tx *gorm.DB,                 // <- the action handler's open transaction
    installation uuid.UUID,
    addonKey, funcName string,
    payload []byte,
    settings map[string]string,
    principal Principal,         // org_id, user_id, locale
) ([]byte, error)
```

`InvokeInTx` stashes `tx` (alongside `principal`) on the per-invocation
context bag (`runtime/wasm/capabilities.go:invocation`). When the guest
calls `db_exec`, the host import:

1. Reads `inv := invocationFrom(ctx)`.
2. Returns `{success:false, error:{code:"no_active_tx"}}` if `inv.tx == nil`.
3. Parses + validates the SQL (§ 10.2) and capability-checks each target
   relation (§ 10.3).
4. Decodes the JSON args (§ 9.6).
5. Opens a `SAVEPOINT addon_<key>_exec_<n>` on `inv.tx` (`n` is the
   per-invocation call counter — capped at 32 per § 10.5). The savepoint
   isolates a single `db_exec` call: a SQLSTATE rollback unwinds **only the
   savepoint**, leaving the surrounding action transaction intact so the
   guest can react to the error and continue. The action itself still
   rolls back if the guest returns `{success:false}` — see § 10.7.
6. Executes the statement on `inv.tx.WithContext(callCtx)` so the
   per-call statement timeout and the cancellation bound by
   `Trigger.TimeoutMs` are honoured.
7. Releases the savepoint on success; rolls back to it on driver error
   and surfaces the JSON error envelope to the guest.

Reusing the *same* `*gorm.DB` handle (rather than opening a sibling
session) is load-bearing for two reasons:

- **Atomicity with the action mutation.** The action handler typically
  performs its own write (e.g. `db.Save(record)`) inside `tx`; pairing the
  guest's `db_exec` writes with the **same** `tx` is what makes the whole
  action atomic.
- **Org filter inheritance.** The host installs an `org_id` GORM scope on
  the action transaction at open time; reusing `tx` means every `db_exec`
  inherits the filter without the guest having to pass `org_id` itself.

The host **never** spawns goroutines that touch `inv.tx` — wazero already
serialises wasm calls per module (`Module.mu`, `wasm.go:151`), so the
single-writer contract of `*gorm.DB` is preserved.

### 10.7 Auto-rollback contract

The transaction lifecycle is owned entirely by the host. The guest cannot
issue `BEGIN` / `COMMIT` / `ROLLBACK` (rejected by § 10.2) and cannot
escape the savepoint window (rejected by the host import).

The host commits the action transaction **iff** all of the following hold
when the top-level guest call returns:

1. The exported function returned a packed pointer to a JSON envelope
   with `"success": true`.
2. No `db_exec` call returned an error envelope to the guest **that the
   guest then re-surfaced** (i.e. the final envelope is `success:true`).
   A guest is free to call `db_exec`, observe a `constraint_violation`,
   and recover — the savepoint kept the surrounding tx alive precisely
   for that purpose.
3. The guest did not panic, abort-trap, exceed `timeout_ms`, exceed
   `memory_limit_mb`, or exceed any of the per-call db limits (§ 10.5).

If **any** of those conditions fails, the host issues `tx.Rollback()` and
surfaces the failure to the action caller as a kernel envelope with
`success:false`. Specifically:

| Trigger                                                                 | Action tx outcome | Envelope returned to caller                                      |
|-------------------------------------------------------------------------|-------------------|-----------------------------------------------------------------|
| Guest returns `{success:true, …}`                                       | **Commit**        | `{success:true, data: …}`                                       |
| Guest returns `{success:false, error: …}`                               | Rollback          | `{success:false, error: <guest's error>, meta:{rolled_back:true}}` |
| Guest panics / abort-traps                                              | Rollback          | `{success:false, error:{code:"runtime_error"}, meta:{rolled_back:true}}` |
| Guest exceeds `timeout_ms`                                              | Rollback          | `{success:false, error:{code:"timeout"}, meta:{rolled_back:true}}` |
| Guest exceeds `memory_limit_mb`                                         | Rollback          | `{success:false, error:{code:"memory_exhausted"}, meta:{rolled_back:true}}` |
| `db_exec` returns `row_limit_exceeded` and guest re-surfaces it         | Rollback          | `{success:false, error:{code:"row_limit_exceeded"}, …}`         |
| `db_exec` returns `serialization_failure` and guest re-surfaces it      | Rollback          | `{success:false, error:{code:"serialization_failure"}, meta:{retryable:true, rolled_back:true}}` — the bridge MAY retry the entire action up to `Trigger.RetryPolicy.MaxAttempts` (default 0). |
| Host context cancelled (request aborted upstream)                       | Rollback          | `{success:false, error:{code:"canceled"}, meta:{rolled_back:true}}` |

Rollback is **always** safe to repeat: the host invokes `tx.Rollback()`
even if a previous error already caused the driver to abort the
transaction, and treats the resulting `sql: transaction has already been
committed or rolled back` as a no-op.

### 10.8 Side-effect ordering

`db_exec` is synchronous and serialised per module instance. Within a
single guest call:

1. Statements execute in the order the guest calls `db_exec`.
2. Each call observes its own and all previous mutations (read-your-writes
   inside the same tx).
3. The host does **not** flush an outbox / event-bus emit until the host
   commits the action tx. Guests that emit domain events via `db_exec`
   (e.g. `INSERT INTO addon_<key>.outbox …`) get the right semantics for
   free: rolled-back actions never publish.

### 10.9 Allowed argument types

Identical to `db_query` (§ 9.6). The same `arg_decode` rule applies to
nested objects/arrays.

### 10.10 Out of scope for v1.2

These are deliberately **not** in v1.2 and will land as separate proposals:

- `COPY FROM STDIN` bulk loads. The streaming surface needs its own ABI.
- `LISTEN` / `NOTIFY`. Guests should publish via the event bus
  (`Trigger.Type="event"`) instead of holding a long-lived notify channel.
- DDL (`CREATE TABLE`, `ALTER TABLE`, …). Schema migrations stay in the
  manifest's `migrations/` directory and are applied by the installer
  (`dynamic/migration.go`). The guest never touches DDL at runtime.
- Cross-installation writes. A guest can only mutate rows belonging to
  its own `installation_id` (enforced by the same `org_id` scope as reads).

### 10.11 Tests (proposed coverage)

- `INSERT INTO addon_<key>.tickets …` resolved bare → runs and commits.
- `INSERT INTO addon_<key>.tickets … RETURNING id` → `returning` populated,
  `lastInsertId` matches.
- Single-row `INSERT` without `RETURNING` → `lastInsertId` populated for
  integer PKs, `null` for uuid PKs without `RETURNING`.
- `UPDATE billing.invoices` without explicit `db:write billing.invoices`
  → `forbidden`.
- `UPDATE addon_<key>.tickets SET …` without `WHERE org_id = $N` → first
  rejected with `missing_org_filter`; same statement with the filter →
  runs.
- `SELECT 1` → `invalid_sql`.
- `BEGIN` / `COMMIT` / `SAVEPOINT foo` → `invalid_sql`.
- Guest invoked via `Host.Invoke` (no tx) → `no_active_tx`.
- Guest returns `{success:false}` after a successful `db_exec` →
  `tx.Rollback()`, no row visible after the action.
- Guest panics inside an exported function after `db_exec` → rollback.
- 33rd `db_exec` call inside one invocation → `db_exec_limit_exceeded`
  (rejected before parse).
- `db_exec` inside an `UPDATE` whose `RETURNING` produces 10 001 rows →
  `row_limit_exceeded`, action rolled back.
- Concurrent action that triggers SQLSTATE 40001 → `serialization_failure`,
  bridge retries when `Trigger.RetryPolicy.MaxAttempts > 0`.

## 11. Implementation notes (kernel-side)

These are non-normative pointers for whoever lands the implementation; they
do not form part of the ABI itself.

### 11.1 Wiring

- New host imports go in `runtime/wasm/capabilities.go` next to
  `http_fetch`. The same `invocationFrom(ctx)` bag carries `addonKey`,
  `caps`, settings, and — for v1.2 — `tx *gorm.DB` plus `principal`.
- `Host` (in `runtime/wasm/wasm.go`) gains a `db DBQuerier` dependency for
  v1.1 (`db_query`) and a `txProvider TxProvider` hook for v1.2 callers
  that prefer to inject a transactional handle from outside the action
  bridge. The runtime stays driver-agnostic so the unit tests can pass an
  in-memory fake.
- The `invocation` struct in `capabilities.go` (`runtime/wasm/capabilities.go:22`)
  grows two new fields: `db DBQuerier` (v1.1, read path) and
  `tx *gorm.DB` (v1.2, write path). `tx` is populated only by the new
  `Host.InvokeInTx` entry point — the legacy `Host.Invoke` leaves it nil
  so any guest call to `db_exec` cleanly fails with `no_active_tx`.
- The action bridge (audit
  [`docs/audits/2026-05-04-action-trigger-gap.md`](audits/2026-05-04-action-trigger-gap.md))
  is the canonical caller of `InvokeInTx`. It opens the transaction,
  installs the `org_id` GORM scope, calls the guest, and commits/rolls
  back per § 10.7.

### 11.2 Parsing

- Use the existing `query/` package's parser if it can be adapted; otherwise
  add a thin wrapper around `pg_query_go` (already an indirect dep via
  `query/builder.go`). Either way the parser must:
  1. Reject if `Stmts` length ≠ 1.
  2. For `db_query`: walk the AST and collect every `RangeVar` (and CTE
     references); reject if the top-level statement is not `SELECT` /
     `WITH … SELECT`.
  3. For `db_exec`: assert the top-level statement is `INSERT` / `UPDATE` /
     `DELETE` / `MERGE` (or `WITH … <mutation>`); reject any tx-control
     verb (`BEGIN`, `COMMIT`, `ROLLBACK`, `SAVEPOINT`, `RELEASE`,
     `SET TRANSACTION`); collect the **target** relation separately from
     **source** relations (the latter only need read capabilities).
  4. For each `RangeVar`, resolve the schema: explicit `schemaname` wins,
     otherwise default to `addon_<key>` (mirrors the `SET LOCAL`
     search_path).
  5. Hand the qualified `<schema>.<table>` to
     `Capabilities.CanReadModel` (sources, db_query) or
     `Capabilities.CanWriteModel` (targets, db_exec).
  6. For `db_exec` mutations on tables that carry an `org_id` column, walk
     the `WHERE` clause and reject when no `org_id = $N` predicate (or
     equivalent equality) is present (`missing_org_filter`).

### 11.3 Transaction shape

For `db_query` (v1.1) the host opens its own short-lived read-only tx:

```sql
BEGIN READ ONLY;
SET LOCAL statement_timeout = '5s';
SET LOCAL search_path TO addon_<key>, public;
-- prepared statement bound to the validated SQL + decoded args
ROLLBACK;  -- always; v1.1 is read-only by contract
```

`READ ONLY` is belt-and-braces against a parser bypass: even if a malicious
addon coaxes a write statement through, the transaction will refuse it.

For `db_exec` (v1.2) the host **does not open a new tx** — it reuses the
action handler's. The action bridge runs:

```go
err := db.Transaction(func(tx *gorm.DB) error {
    // SET LOCAL once at tx open time so every db_query/db_exec the guest
    // issues inherits the right search_path and statement_timeout.
    if err := tx.Exec("SET LOCAL search_path TO addon_" + key + ", public").Error; err != nil {
        return err
    }
    if err := tx.Exec("SET LOCAL statement_timeout = '5s'").Error; err != nil {
        return err
    }
    // Persist the action's own row mutation.
    if err := tx.Save(record).Error; err != nil {
        return err
    }
    // Hand control to the guest. db_exec calls inside this scope land on
    // savepoints owned by `tx`; a returned error rolls back everything.
    out, err := host.WASM.InvokeInTx(ctx, tx, install, addonKey, fn, payload, settings, principal)
    if err != nil {
        return err
    }
    return interpretGuestEnvelope(out) // returns non-nil on success:false
})
```

The `db.Transaction` closure form is preferred over manual `Begin`/`Commit`
because it normalises panic recovery: GORM converts a panic into a
rollback before re-raising, which dovetails with § 10.7.

### 11.4 Savepoint per `db_exec` call

```sql
SAVEPOINT addon_<key>_exec_<n>;
-- prepared statement bound to validated SQL + decoded args
-- on success:
RELEASE SAVEPOINT addon_<key>_exec_<n>;
-- on driver error:
ROLLBACK TO SAVEPOINT addon_<key>_exec_<n>;
```

Use `tx.SavePoint(name)` / `tx.RollbackTo(name)` from GORM rather than raw
SQL so the in-memory tx state matches the wire state. The savepoint name
embeds the addon key and a per-invocation counter to keep nested action
chains debuggable.

### 11.5 Envelope construction

Reuse `httpx.Envelope` (or whatever the `{success, data, meta}` helper is
named in `httpx/`) so the wire shape matches every other kernel handler.
Do not re-roll the JSON encoding inline. The same helper writes both the
`db_query` and `db_exec` envelopes; only the `data` payload differs.

### 11.6 Tests (proposed coverage — `db_query`)

- Bare table → resolves to `addon_<key>` and runs.
- `addon_<key>.tickets` (qualified) → runs.
- `users` with `db:read users` → runs; without → `forbidden`.
- `pg_class` → `forbidden` regardless of capabilities.
- `INSERT INTO …` → `invalid_sql` (also verified by the `READ ONLY` tx).
- `SELECT 1; SELECT 2` → `invalid_sql`.
- `$1` placeholder with empty args → `arg_count_mismatch`.
- 10 001-row result set → `row_limit_exceeded`.
- Statement that sleeps past 5 s → `query_timeout`.

### 11.7 Tests (proposed coverage — `db_exec`)

See § 10.11 for the functional matrix. Additional kernel-side tests:

- `Host.Invoke` (no tx) + guest calls `db_exec` → `no_active_tx`,
  module instance survives so subsequent calls within the same
  invocation still work.
- Two `db_exec` calls on the same `tx`: first succeeds, second hits
  `constraint_violation` → savepoint rollback leaves the first call's
  effects visible to a third call within the same invocation.
- Guest returns `{success:true}` after a recovered `constraint_violation`
  → action commits, only the first call's effects persist.
- Action handler panics **after** `InvokeInTx` returns successfully →
  `db.Transaction` closure rolls back, no rows committed.
- Concurrent action triggering SQLSTATE 40001 → `serialization_failure`
  surfaced; bridge retries when `Trigger.RetryPolicy.MaxAttempts > 0`.

## 12. `event_emit` — domain-event publish (v1.3)

`event_emit` is the dedicated publish import. It is the WASM equivalent of a
host-side `events.Bus.Publish(ctx, addonKey, event, orgID, payload)` call —
intentionally narrow: one event name, one JSON payload, fan-out to whatever
subscribers the kernel has registered. The host owns the bus, the capability
check, and the tenant scoping; the guest only contributes name + bytes.

### 12.1 Signature

```
event_emit(eventPtr i32, eventLen i32, payloadPtr i32, payloadLen i32) -> i64
```

| Param        | Type | Meaning                                                              |
|--------------|------|----------------------------------------------------------------------|
| `eventPtr`   | i32  | Guest pointer to the event name (UTF-8).                             |
| `eventLen`   | i32  | Event-name length in bytes. Hard cap: 256.                           |
| `payloadPtr` | i32  | Guest pointer to the payload buffer. May be `0` for empty payloads.  |
| `payloadLen` | i32  | Payload length in bytes. `0` for empty. Hard cap: 256 KiB (§ 12.5).  |
| **return**   | i64  | Packed `(ptr<<32)\|len` of the response envelope (§ 12.4).           |

A return of `0` is reserved and currently never produced — `event_emit`
always allocates an envelope, even when the publish fanned out to zero
subscribers.

### 12.2 Event-name contract

The event name is the same string the addon would use in
`manifest.capabilities[].target` for an `event:emit` declaration. The host
parses it under the rules below before any capability check runs:

- **Non-empty.** Empty buffer → `invalid_event` (`reason: "empty_name"`).
- **UTF-8.** Invalid encoding → `invalid_event` (`reason: "not_utf8"`).
- **Length ≤ 256 bytes.** Over-long names → `invalid_event`
  (`reason: "name_too_long"`).
- **Shape.** Lowercase ASCII identifier segments separated by dots:
  `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`. The bus matcher only honours
  trailing `.*` wildcards on **subscribe** patterns; publishing a wildcard
  (`tickets.*`) is rejected with `invalid_event` (`reason:
  "wildcard_publish"`) so a single rogue addon cannot fan out to every
  subscriber of a prefix.
- **No leading / trailing dot, no double dots.** Rejected with
  `invalid_event` (`reason: "malformed"`).

### 12.3 Payload contract

The payload is opaque bytes from the host's point of view. We document a
canonical shape so subscribers (kernel handlers, other addons, action
bridges) can decode without prior arrangement:

- **Recommended JSON.** When the payload is non-empty the guest SHOULD send
  a UTF-8 JSON document. The host does not parse it — it is forwarded
  verbatim — but tooling (event log, audit, replay) assumes JSON.
- **Empty payload allowed.** `payloadPtr = payloadLen = 0` is valid and
  delivered as an explicit `nil` to subscriber handlers.
- **Binary blobs.** If a guest needs to publish raw bytes, wrap them in
  `{"$bytes": "<base64>"}` to mirror the `db_query` arg encoding (§ 9.6).
- **Size cap.** Payloads larger than the limit in § 12.5 are rejected with
  `payload_too_large` before any capability check runs.

The host materialises the call to `events.Bus.Publish` as:

```go
var payload any
if len(buf) > 0 {
    payload = json.RawMessage(buf) // forwarded verbatim; subscribers decode
}
err := bus.Publish(ctx, addonKey, eventName, orgID, payload)
```

`json.RawMessage` keeps the bytes immutable for the synchronous fan-out and
lets subscribers either treat the payload as opaque or `json.Unmarshal` it
into a typed struct.

### 12.4 Response envelope

Success follows the kernel `{success, data, meta}` convention:

```json
{
  "success": true,
  "data": {
    "event":       "tickets.resolved",
    "subscribers": 3
  },
  "meta": {
    "addon":      "tickets",
    "orgId":      "11111111-1111-1111-1111-111111111111",
    "durationMs": 1
  }
}
```

- `data.subscribers` is the number of handlers `events.Bus.Publish` invoked.
  Returns `0` when no pattern matched the event name — that is a successful
  publish, not an error.
- `meta.orgId` is the tenant id the host bound to the invocation (§ 12.6).
  It is informational; it is **not** part of the addon's per-call surface
  and cannot be overridden from the guest.
- `meta.durationMs` covers the synchronous fan-out (every subscriber
  handler runs serially inside `Publish`).

Errors share the same outer shape and never propagate subscriber failures —
those are logged by the bus itself:

```json
{
  "success": false,
  "error":   { "code": "forbidden",
               "message": "addon \"tickets\" lacks event:emit \"orders.refunded\"" },
  "meta":    { "addon": "tickets", "durationMs": 0 }
}
```

Defined error codes:

| Code                | When                                                                |
|---------------------|---------------------------------------------------------------------|
| `invalid_event`     | Event-name parse failure (see § 12.2 sub-reasons).                  |
| `payload_too_large` | Payload exceeded the cap from § 12.5.                               |
| `forbidden`         | `Capabilities.CanEmit(event)` denied the publish.                   |
| `no_active_org`     | Invocation reached the host import without an `orgID` (§ 12.6).     |
| `bus_unavailable`   | Host was constructed without an `events.Bus` (deployment misconfig).|

Subscriber-side errors do not appear here. `events.Bus.Publish` already
swallows handler errors and logs them; the publisher is decoupled from
delivery failures by design.

### 12.5 Limits

| Knob                    | Default | Configurable via                                  |
|-------------------------|---------|---------------------------------------------------|
| Max event-name length   | 256 B   | host-side (`runtime/wasm` config).                |
| Max payload size        | 256 KiB | host-side. Tighter than the 8 MiB `http_fetch`/`db_query` cap because the bus fan-out is synchronous and in-memory. |
| Max `event_emit` calls per invocation | 64 | host-side. Caps blast radius of a runaway publisher. |
| Max response bytes      | 1 KiB   | host-side. Envelope is tiny by construction; cap is just a guard. |

The per-call deadline of the surrounding invocation (`Trigger.TimeoutMs` /
`BackendSpec.TimeoutMs`) still bounds the total wall time of the fan-out,
but `event_emit` does not impose its own statement timeout — handlers that
need to bound their own work do so themselves.

### 12.6 Tenant scope (`orgID` from the context bag)

`event_emit` always carries an `orgID uuid.UUID` to `events.Bus.Publish`.
The guest cannot supply it: the host stashes it on the per-invocation
context bag (`runtime/wasm/capabilities.go:invocation`) at `Host.Invoke`
entry time and the import reads it back via `invocationFrom(ctx)`.

```go
inv := invocationFrom(ctx)
if inv == nil || inv.orgID == uuid.Nil {
    return writeToGuest(ctx, mod, jsonError("no_active_org",
        "invocation has no bound orgID"))
}
```

The lookup mirrors what HTTP handlers do via `httpx.ExtractOrgID` — the
caller of `Host.Invoke` (action bridge, webhook adapter, dynamic CRUD hook)
is responsible for resolving `LocalOrganizationID` from its own context and
forwarding it. Calling `Host.Invoke` without an `orgID` is a wiring bug; we
surface it as `no_active_org` instead of letting the bus publish under a
nil tenant.

The bus itself does not filter handlers by `orgID` — it only matches on
event-name patterns. Subscribers receive `(ctx, orgID, payload)` and are
expected to use `orgID` for their own scoping (e.g. only act on rows that
belong to that tenant). That contract is unchanged from `events.Bus`.

### 12.7 Capability enforcement

Capability check is delegated to the bus, not duplicated in the import:

```go
err := bus.Publish(ctx, addonKey, eventName, orgID, payload)
// Publish internally calls:
//   enforcer.CheckCapability(addonKey, "event:emit", eventName)
//   → Capabilities.CanEmit(eventName)
```

Consequences:

- **Wildcards in declarations work as documented.** An addon that declared
  `event:emit tickets.*` may publish `tickets.created`, `tickets.resolved`,
  etc., because `Capabilities.CanEmit` already walks the same matcher used
  for subscribe patterns.
- **Shadow / enforce mode is consistent.** When the enforcer is in
  `ModeShadow`, a missing capability logs the violation but does not block
  the publish — same behaviour the addon would observe from any host-side
  caller. The `event_emit` envelope still reports `success: true` in that
  case, mirroring the bus.
- **Kernel-trusted publishes are still possible.** When the kernel itself
  needs to fan out (e.g. from `dynamic.Service.publishCanonical`, see audit
  [`docs/audits/2026-05-04-dynamic-events.md`](audits/2026-05-04-dynamic-events.md)),
  it calls the bus directly with `addonKey="kernel"`; that path bypasses
  the capability check on purpose. `event_emit` never sets `addonKey =
  "kernel"` — the host import always passes `inv.addonKey`.

### 12.8 Side-effect ordering

`event_emit` is synchronous and serialised per module instance — wazero
already serialises calls per module via `Module.mu` (`wasm.go:151`), so:

1. Statements within a single guest call execute in the order the guest
   issues `event_emit`. There is no batching, no async dispatch.
2. Each call observes the side effects of every prior call (subscriber
   handlers ran before the next `event_emit` returns).
3. The bus has **no outbox semantics today** — publishes are not bound to
   any database transaction. A guest that wants
   "publish-on-commit" semantics must publish via `db_exec INSERT INTO
   addon_<key>.outbox` and rely on the bridge's outbox flusher (out of
   scope for v1.3; see § 12.10).
4. Publishes from inside an `InvokeInTx` action handler are NOT held until
   commit. If the surrounding action rolls back, subscribers may have
   already reacted to the event. Treat publish from inside an action as
   "fire-and-forget"; if you need the rollback guarantee, use the outbox
   pattern.

### 12.9 Wiring proposal

The implementation lives in `runtime/wasm/capabilities.go` next to
`http_fetch`. It carries the same shape as the existing imports:

- `Host` (in `runtime/wasm/wasm.go`) gains a `bus *events.Bus` dependency
  injected at construction time. `NewHost` grows an `events.Bus` parameter
  (or accepts a functional option to keep the legacy constructor intact —
  see § 12.10).
- The `invocation` struct (`runtime/wasm/capabilities.go:22`) grows an
  `orgID uuid.UUID` field. `Host.Invoke` is extended (or paired with a
  `Host.InvokeFor(ctx, orgID, …)` sibling) so callers always thread the
  tenant id through.
- `registerHostModule` registers a new `event_emit` builder that:
  1. Reads `inv := invocationFrom(ctx)`; returns `bus_unavailable` if
     `inv == nil` or `inv.bus == nil`.
  2. Reads + validates the event name (§ 12.2). Rejects wildcards.
  3. Reads + size-checks the payload (§ 12.3, § 12.5).
  4. Returns `no_active_org` if `inv.orgID == uuid.Nil`.
  5. Calls `inv.bus.Publish(ctx, inv.addonKey, eventName, inv.orgID,
     json.RawMessage(payload))`.
  6. Builds the success envelope (with `data.subscribers` taken from a
     `Bus.PublishWithCount` helper — see § 12.10) and writes it back to
     the guest via `writeToGuest`.

The `event_emit` import does NOT need access to `inv.tx`. Publish is
purely an in-process, in-memory fan-out; it never opens a DB transaction.

### 12.10 Out of scope for v1.3

- **Subscribe from inside a guest.** No `event_subscribe` host import in
  v1.3. Guests register subscriptions declaratively via `manifest.handlers`
  (event triggers); a runtime `event_subscribe` would have to deal with
  guest-side handler lifetimes across instance recycling and is not
  required by the current ecosystem.
- **Publish-on-commit (outbox).** When `event_emit` is called from an
  `InvokeInTx` guest, the publish fires synchronously. A future minor
  version may stage publishes on `inv.tx` and flush them only after the
  bridge commits.
- **Cross-process bus.** v1.3 still targets the in-process `events.Bus`.
  When the bus grows a NATS / Kafka transport, the host import surface
  stays the same — only the receiving end changes.
- **`Bus.PublishWithCount` helper.** `events.Bus.Publish` currently returns
  only `error` — landing the v1.3 envelope's `data.subscribers` cleanly
  needs a sibling method (or a `(int, error)` return). That extension is
  trivial but lives in the kernel-side implementation PR, not in this ABI
  proposal.

### 12.11 Tests (proposed coverage — `event_emit`)

Functional matrix to be exercised once the import lands. All cases assume
the guest is invoked through the proposed `Host.Invoke(..., orgID, ...)`
entry point unless otherwise noted.

- **Happy path, declared capability.** Addon declares `event:emit
  tickets.*`. Guest emits `tickets.resolved` with a JSON payload; one
  subscriber registered → envelope `{success:true, data:{event:
  "tickets.resolved", subscribers:1}}`, subscriber receives the right
  `(orgID, payload)`.
- **Happy path, no subscribers.** Same capability, no subscriber for
  `tickets.archived` → envelope `{success:true, data:{subscribers:0}}`.
- **Wildcard publish denied.** Guest emits `tickets.*` (literal asterisk in
  the name) → `invalid_event` (`reason: "wildcard_publish"`).
- **Empty event name.** `eventLen=0` → `invalid_event` (`reason:
  "empty_name"`).
- **Malformed event name.** Capital letters / leading dot / double dot →
  `invalid_event` (`reason: "malformed"`).
- **Over-long event name.** 257-byte name → `invalid_event` (`reason:
  "name_too_long"`).
- **Non-UTF-8 event name.** Random bytes that fail `utf8.Valid` →
  `invalid_event` (`reason: "not_utf8"`).
- **Capability missing in enforce mode.** Addon declares no event
  capabilities; enforcer in `ModeEnforce` → `forbidden`,
  `data.subscribers` not populated.
- **Capability missing in shadow mode.** Same setup with
  `ModeShadow` → `success:true, data.subscribers:0`, log line records the
  violation. Mirrors the bus's existing behaviour.
- **No active org.** Caller forgot to thread `orgID` — `Host.Invoke` is
  invoked with `uuid.Nil` → `no_active_org`. Module instance survives so
  later imports keep working.
- **Bus unavailable.** `Host` constructed without a `*events.Bus` →
  `bus_unavailable`. Mirrors the deployment-misconfig path the dynamic
  audit calls out (`docs/audits/2026-05-04-dynamic-events.md`).
- **Payload too large.** `payloadLen = 256 KiB + 1` → `payload_too_large`.
- **Empty payload.** `payloadPtr=payloadLen=0`, declared capability →
  envelope `success:true`; subscriber's `payload any` parameter is `nil`.
- **Binary payload.** Guest emits with `{"$bytes":"<base64>"}` body —
  forwarded verbatim, subscriber decodes.
- **Concurrency.** Two installations of the same addon publish on the same
  pattern simultaneously; subscriber ordering is FIFO per installation,
  mutual ordering is not guaranteed. The test asserts both publishes
  reach every subscriber and `data.subscribers` per envelope is correct.
- **Call cap.** 65th `event_emit` call within one invocation →
  `event_emit_limit_exceeded` (rejected before any name validation).
- **Subscriber error swallowed.** Subscriber handler returns an error →
  publisher still sees `{success:true, data:{subscribers:N}}`; bus logs
  the handler error.
- **Kernel trusted-path unaffected.** Independent test asserts that direct
  `bus.Publish(ctx, "kernel", evt, orgID, payload)` continues to bypass
  the capability check (regression guard for § 12.7).
