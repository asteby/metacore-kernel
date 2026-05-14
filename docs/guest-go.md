# Guest-side helpers (Go / TinyGo)

The kernel ships a small Go package — `github.com/asteby/metacore-kernel/guest`
— with TinyGo-compatible helpers that addon authors can import from inside
a wasm backend instead of hand-rolling the `(ptr, len) -> i64` ABI on every
host import.

This is a follow-up to the kernel-side change that made
[`event_emit`](wasm-abi.md#124-response-envelope) return a rich
`{success, data, meta}` envelope (kernel PR #62). Without these helpers
every addon had to repeat the same boilerplate: marshal payload, unpack the
i64, locate the buffer in guest memory, JSON-decode the envelope and
branch on `success`. The helpers do that once.

> **Scope (today).** `EmitEvent`, `Log`, `EnvGet`, `HttpFetch`,
> `DbQuery` and `DbExec` — full coverage of the six host imports
> documented in [`docs/wasm-abi.md` § 3](wasm-abi.md#3-host-imports-module-metacore_host).

---

## 1. Install

The package lives inside the kernel module, so addons that already pin
the kernel pick it up for free:

```bash
go get github.com/asteby/metacore-kernel@latest
```

The helpers compile under both `tinygo` (target `wasi` or `wasm-unknown`)
and `go` (for unit tests on the host). Only stdlib (`encoding/json`,
`errors`, `unsafe`) is touched — no third-party deps.

## 2. EmitEvent

```go
import "github.com/asteby/metacore-kernel/guest"

//go:export resolve_ticket
func resolve_ticket(ptr, n uint32) uint64 {
    // ... your logic ...
    result, err := guest.EmitEvent("tickets.resolved", map[string]any{
        "id":   "8b1f...",
        "user": "u_42",
    })
    if err != nil {
        // Typed branching is supported via errors.As:
        var emErr *guest.EmitEventError
        if errors.As(err, &emErr) {
            switch emErr.Code {
            case "forbidden":
                // capability missing — surface to caller verbatim
            case "invalid_event":
                // developer-side bug, log loudly
            case "no_active_org":
                // host wiring bug — caller forgot orgID
            }
        }
        return 0
    }
    // Notify operations dashboard, telemetry, etc.
    fmt.Printf("notified %d subscribers in %dms\n",
        result.Subscribers, result.Meta.DurationMs)
    return 0
}
```

### 2.1 Return shape

```go
type EmitEventResult struct {
    Event       string         // canonical event name the host accepted
    Subscribers int            // number of handlers the bus fanned out to
    Meta        EmitEventMeta  // host bookkeeping (orgId, emittedAt, durationMs, envelopeVersion)
}
```

A successful publish that matched **zero** subscribers is **not** an
error — it returns `EmitEventResult{Subscribers: 0}` with `err == nil`.
This mirrors the host's `events.Bus.Publish` contract.

### 2.2 Error surface

```go
type EmitEventError struct {
    Code    string         // documented error code (see below)
    Message string         // human-readable detail
    Meta    EmitEventMeta  // populated even on errors — durationMs/emittedAt usable
}
```

Defined codes (forwarded verbatim from
[`docs/wasm-abi.md` § 12.4](wasm-abi.md#124-response-envelope)):

| Code                | Meaning                                                              |
|---------------------|----------------------------------------------------------------------|
| `invalid_event`     | event name failed validation (empty, malformed, wildcard publish…)   |
| `payload_too_large` | payload over 256 KiB                                                 |
| `forbidden`         | addon lacks `event:emit <name>` capability                           |
| `no_active_org`     | host wiring bug — caller of `Host.Invoke` forgot `orgID`             |
| `bus_unavailable`   | host built without an `events.Bus` (deployment misconfig)            |
| `invalid_payload`   | payload bytes weren't valid UTF-8 / out of guest memory              |
| `unknown`           | host returned `success:false` without an `error.code` (defensive)    |

The helper additionally produces `invalid_payload` locally when
`encoding/json.Marshal` fails on the supplied Go value — surfacing the
JSON encoder's error message — so addons don't need a separate
"could the payload even encode" branch.

### 2.3 Envelope version

The helper was authored against envelope **v1**
(`SupportedEmitEnvelopeVersion = 1`). Decoding is forward-compatible:
future hosts that bump `meta.envelopeVersion` to 2 (and add new
fields) keep working — unknown fields are ignored, known fields are
populated.

If you want to log a warning when the host outpaces the SDK:

```go
if result.Meta.EnvelopeVersion > guest.SupportedEmitEnvelopeVersion {
    // host shipped a newer envelope; consider upgrading the SDK
}
```

## 3. The `alloc` export

Every wasm backend MUST export `alloc(size i32) i32` so the host can
write response envelopes into guest memory before returning. The
contract is documented in [`docs/wasm-abi.md` § 2](wasm-abi.md#2-required-guest-exports).

The `guest` package ships a default bump allocator (a single
`make([]byte, size)` per call). It is **opt-in** via build tag — most
addons already have their own `alloc`, so we don't shadow it by
default:

```bash
tinygo build -target=wasi -opt=z -no-debug \
    -tags metacore_guest_alloc \
    -o backend/backend.wasm ./backend/
```

If you already have an `alloc` somewhere else in your module, omit the
tag.

## 4. Building

### 4.1 TinyGo

```bash
tinygo build -target=wasi -opt=z -no-debug \
    -o backend/backend.wasm ./backend/
```

Tested against TinyGo 0.30+ — the helpers only use `encoding/json`,
`errors`, `unsafe`, all of which TinyGo supports.

### 4.2 Plain Go (host-side tests)

```bash
go test ./guest/...
```

Type definitions, decoder logic and the error type compile on every
target. The wasm-specific files (`alloc_wasm.go`, `eventemit_wasm.go`)
are guarded by `//go:build wasm || wasip1` so `go build` on a Linux
host stays a pure type-check.

## 5. Log

```go
guest.Log(guest.LevelInfo, "ticket resolved")
guest.Log(guest.LevelWarn, "retry exhausted, falling back")
```

`Log` wraps `metacore_host.log` (see
[`docs/wasm-abi.md` § 3](wasm-abi.md#3-host-imports-module-metacore_host)).
The host import accepts a single string; the helper prepends
`[<level>] ` so structured log pipelines can branch on severity
without an ABI change. The call is fire-and-forget — there is no
envelope to decode and `Log` never returns an error.

### 5.1 Levels

```go
type LogLevel int
const (
    LevelDebug LogLevel = iota
    LevelInfo
    LevelWarn
    LevelError
)
```

Unknown enum values render as `info` on the wire so a future enum
bump cannot corrupt the host's structured log fields.

## 6. EnvGet

```go
apiKey, found, err := guest.EnvGet("STRIPE_API_KEY")
if err != nil {
    // reserved — today always nil; future host may add a typed
    // capability error.
}
if !found {
    // settings map didn't carry the key, or the value was empty.
}
```

`EnvGet` reads an installation setting (or secret) by key. The host
returns the raw value bytes directly — there is no JSON envelope.

The helper folds "key missing" and "key present but empty" into a
single `found == false` because the host today (see
[`runtime/wasm/capabilities.go:134-137`](../runtime/wasm/capabilities.go))
treats them identically. The third return slot is reserved for a
future host that surfaces a typed envelope (e.g.
`{success:false, error:{code:"forbidden"}}`); callers should keep
the `err` check to avoid a breaking signature change.

## 7. HttpFetch

```go
resp, err := guest.HttpFetch(guest.HttpRequest{
    Method: "POST",
    URL:    "https://api.stripe.com/v1/refunds",
    Body:   []byte(`{"charge":"ch_..."}`),
})
if err != nil {
    var denied *guest.HttpCapabilityDeniedError
    if errors.As(err, &denied) {
        // addon manifest is missing `http:fetch api.stripe.com`
    }
    var fe *guest.HttpFetchError
    if errors.As(err, &fe) {
        switch fe.Code {
        case "transport":   /* retry */
        case "bad_request": /* surface as bug */
        }
    }
    return 0
}
fmt.Printf("upstream returned %d, %d bytes body\n", resp.Status, len(resp.Body))
```

`HttpFetch` wraps `metacore_host.http_fetch`. The host enforces the
addon's `http:fetch <host-whitelist>` capability + egress SSRF guard
before any syscall happens — see
[`docs/permissions.md`](permissions.md).

### 7.1 Shapes

```go
type HttpRequest struct {
    Method  string              // "GET" by default
    URL     string              // absolute URL
    Headers map[string][]string // reserved; host ignores today
    Body    []byte              // nil for GET / empty body
}

type HttpResponse struct {
    Status  int                  // HTTP status code
    Headers map[string][]string  // reserved; nil today
    Body    []byte               // capped at 8 MiB by host
}
```

`Headers` on both shapes is reserved — the host today only forwards
the method, URL and body and hard-codes `Content-Type:
application/json` when the body is non-empty
([`runtime/wasm/capabilities.go:165-167`](../runtime/wasm/capabilities.go)).
Populating the field is silently ignored — preserved so consumers
don't refactor when the host gains header support.

### 7.2 Error surface

| Code             | Meaning                                                              |
|------------------|----------------------------------------------------------------------|
| `forbidden`      | `Capabilities.CanFetch` denied — surfaced as `*HttpCapabilityDeniedError` |
| `bad_request`    | host couldn't build the request (invalid URL, bad method)            |
| `transport`      | DNS / dial / TLS / read-body failure — retry-safe                    |
| `decode`         | host wrote something the helper couldn't parse (host bug)            |

## 8. DbQuery

```go
res, err := guest.DbQuery(
    `SELECT id, status FROM tickets WHERE org_id = $1 AND status = $2`,
    orgID, "open",
)
if err != nil {
    var qErr *guest.DbQueryError
    if errors.As(err, &qErr) {
        switch qErr.Code {
        case "forbidden":         /* missing db:read capability */
        case "row_limit_exceeded":/* paginate */
        case "query_timeout":     /* simplify */
        }
    }
    return 0
}
for _, row := range res.Rows {
    fmt.Printf("ticket %v status=%v\n", row["id"], row["status"])
}
fmt.Printf("scanned %d rows in %dms\n", res.RowCount, res.Meta.DurationMs)
```

`DbQuery` wraps `metacore_host.db_query` (see
[`docs/wasm-abi.md` § 9](wasm-abi.md#9-db_query--scoped-read-only-sql-v11)).
The host enforces SELECT-only contract, AST-level cross-schema
capability gating (`db:read`), row limit (10 000) and per-call
deadline (5 s) — see § 9.2 - 9.5.

Args use Postgres-style positional placeholders (`$1`, `$2`, …);
allowed types follow § 9.6 (nil, bool, integers, floats, strings, plus
the `{$bytes, $uuid, $ts}` JSON wrappers).

### 8.1 Shapes

```go
type QueryResult struct {
    Rows     []map[string]any // decoded result set
    Columns  []ColumnMeta     // {Name, Type}
    RowCount int              // == len(Rows) — host caps over-cap reads as row_limit_exceeded
    Meta     QueryMeta        // {Schema, DurationMs, Truncated}
}

type DbQueryError struct {
    Code    string  // see § 9.4
    Message string
    Meta    QueryMeta
}
```

## 9. DbExec

```go
res, err := guest.DbExec(
    `UPDATE refunds SET status = $1 WHERE id = $2 AND org_id = $3 RETURNING amount_cents`,
    "refunded", refundID, orgID,
)
if err != nil {
    var eErr *guest.DbExecError
    if errors.As(err, &eErr) {
        switch eErr.Code {
        case "constraint_violation":   /* eErr.SQLState carries 23xxx */
        case "serialization_failure":  /* retry the whole action */
        case "missing_org_filter":     /* WHERE didn't constrain org_id */
        case "no_active_tx":           /* host wired Invoke instead of InvokeInTx */
        }
    }
    return 0
}
for _, row := range res.Rows {
    fmt.Printf("refunded amount=%v\n", row["amount_cents"])
}
```

`DbExec` wraps `metacore_host.db_exec` (see
[`docs/wasm-abi.md` § 10](wasm-abi.md#10-db_exec--addon-scoped-writes-v12)).
Accepts `INSERT`/`UPDATE`/`DELETE`/`MERGE` (and leading-CTE variants).
The call piggybacks on the action handler's open transaction when the
host entered through `Host.InvokeInTx`; a guest invoked via plain
`Host.Invoke` receives `no_active_tx`.

`RETURNING` clauses populate `Rows` + `Columns` (kernel v0.11.0+);
plain mutations carry only `RowsAffected`.

### 9.1 Shapes

```go
type ExecResult struct {
    RowsAffected int64
    Rows         []map[string]any // populated only on RETURNING
    Columns      []ColumnMeta     // populated only on RETURNING
    Meta         ExecMeta         // {Schema, DurationMs}
}

type DbExecError struct {
    Code     string  // see § 10.4
    Message  string
    SQLState string  // SQLSTATE preserved for constraint/serialization
    Meta     ExecMeta
}
```

`SQLState` is populated for driver-level violations
(`constraint_violation`, `serialization_failure`, `db_error`) — the
host preserves Postgres SQLSTATE so guests can branch on the
underlying violation without parsing the message.

## 10. ABI version compatibility

| Helper API     | Kernel envelope | Wire-compat note                                       |
|----------------|-----------------|--------------------------------------------------------|
| `EmitEvent` v1 | v1              | Pre-PR#62 hosts that returned literal `0` on success are still supported — `decodeEmitEnvelope(nil)` produces a zero-value `EmitEventResult` with `err == nil`. |
| `Log` v1       | n/a             | Fire-and-forget; no envelope.                                                                                  |
| `EnvGet` v1    | raw bytes       | Host returns the value bytes verbatim; helper folds missing + empty into `found == false`.                     |
| `HttpFetch` v1 | `{status, body}` | Host envelope is flat (no `{success, data, meta}`); helper probes for the `{error, message}` failure shape first. |
| `DbQuery` v1   | v1              | Decoder follows `{success, data, meta}` from § 9.4 verbatim.                                                   |
| `DbExec` v1    | v1 (v0.11+)     | RETURNING projection lives under `data.rows` + `data.columns`; pre-v0.11 envelopes (no `rows`) decode cleanly with `Rows == nil`. |

The helpers are **additive** — addons compiled against the raw host
imports keep working untouched; new addons opt in by calling the
typed wrappers.
