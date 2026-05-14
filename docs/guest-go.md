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

> **Scope (today).** `EmitEvent` only. `db_query`, `db_exec`, `http_fetch`
> and `log` are deliberately out of scope for the first cut — see
> [§ 5](#5-roadmap--what-comes-next) for the order we plan to land them.

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

## 5. Roadmap — what comes next

The kernel ships five host imports today
(`log`, `env_get`, `http_fetch`, `db_query`, `db_exec`, `event_emit`).
`EmitEvent` is the first helper to land because:

1. `event_emit` is the import whose v1.3 envelope (`{success, data,
   meta}`) is materially harder to consume by hand — the older imports
   either return a plain string (`env_get`) or are trivial (`log`).
2. The kernel-side envelope change (PR #62) explicitly called out
   "guest-side helpers, not in the kernel" as the follow-up.

Order we plan to land the rest (no PRs open yet):

- `Log(msg string)` — wraps `metacore_host.log`. Trivial wrapper around
  `stringRef`. Mainly value-add: a structured logger that prepends the
  call-site, similar to `slog`. Low priority.
- `Env(key string) (string, bool)` — wraps `env_get`. Returns
  `("", false)` when the key is missing.
- `Fetch(req FetchRequest) (FetchResponse, error)` — wraps
  `http_fetch`. Mirrors the response shape `{status, body}` documented
  in `runtime/wasm/capabilities.go`.
- `Query(sql string, args ...any) (QueryResult, error)` — wraps
  `db_query`. The response envelope already follows
  `{success, data, meta}`, so this reuses the same decoder skeleton as
  `EmitEvent`.
- `Exec(sql string, args ...any) (ExecResult, error)` — wraps
  `db_exec`. Same envelope shape; surfaces `rowsAffected` /
  `lastInsertId` / `returning`.

Each will land as a separate PR with its own test matrix. See the
package doc comment in `guest/doc.go` for the canonical list.

## 6. ABI version compatibility

| Helper API     | Kernel envelope | Wire-compat note                                       |
|----------------|-----------------|--------------------------------------------------------|
| `EmitEvent` v1 | v1              | Helper handles the post-PR#62 envelope. Pre-PR#62 hosts that returned literal `0` on success are still supported — `decodeEmitEnvelope(nil)` produces a zero-value `EmitEventResult` with `err == nil`. |

The helper is **additive** — addons compiled against the older raw
`hostEventEmit` import keep working untouched; new addons opt in by
calling `guest.EmitEvent`.
