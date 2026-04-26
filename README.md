<p align="center">
  <img src="docs/assets/metacore.svg" width="120" alt="Metacore Kernel" />
</p>

<h1 align="center">Metacore Kernel</h1>

<p align="center">
  <em>Secure WASM runtime and substrate for declarative, multi-tenant Go applications.</em>
</p>

<p align="center">
  <a href="https://go.dev/dl/"><img alt="Go 1.25" src="https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white" /></a>
  <a href="https://github.com/asteby/metacore-kernel/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/asteby/metacore-kernel/actions/workflows/ci.yml/badge.svg?branch=main" /></a>
  <a href="https://github.com/asteby/metacore-kernel/actions/workflows/release.yml"><img alt="Release" src="https://github.com/asteby/metacore-kernel/actions/workflows/release.yml/badge.svg" /></a>
  <img alt="License" src="https://img.shields.io/badge/license-proprietary-lightgrey" />
</p>

> 📚 **Documentation:** Full docs live at **[asteby.github.io/metacore](https://asteby.github.io/metacore/)**. The `docs/` folder in this repo is kept for offline reference only.

---

## Table of contents

- [What is `metacore-kernel`](#what-is-metacore-kernel)
- [Dynamic CRUD framework](#dynamic-crud-framework)
- [Architecture](#architecture)
- [Embedding](#embedding)
- [Subsystems](#subsystems)
- [WebSocket protocol](#websocket-protocol)
- [Addon contract](#addon-contract)
- [Documentation](#documentation)
- [Development](#development)
- [Release process](#release-process)
- [Module path and private access](#module-path-and-private-access)
- [License](#license)

---

## What is `metacore-kernel`

`metacore-kernel` is the **runtime substrate** that every Metacore application
embeds as a Go module. It owns the cross-cutting concerns no individual product
should re-implement:

- **WASM addon runtime** — compiles and executes signed addons in a wazero
  sandbox with memory caps, wall-clock timeouts, and a host module that gates
  every privileged syscall through a capability enforcer.
- **Capability-based security** — HMAC-signed install secrets, nonce-based
  replay protection, and a global enforcer with `shadow`/`enforce` modes that
  operators flip at runtime.
- **Lifecycle orchestration** — install / enable / disable / uninstall hooks,
  plus a `Boot()` phase that wires shared services (DB, event bus, …) into
  every registered addon.
- **Metadata-driven CRUD** — a registry of `TableMetadata` / `ModalMetadata`
  feeds a generic dynamic CRUD service that every host shares; apps register
  models, the kernel exposes the routes.
- **Event bus, WebSocket hub, push, webhooks** — the technical communication
  primitives every multi-tenant web application eventually needs, framework-
  agnostic at the service layer and Fiber-native at the handler layer.

The kernel is **embeddable**, not a server. It compiles into the host binary
(`link`, `ops`, …); there is no `metacore` daemon. All public contracts are
behind interfaces, so adding fields to base structs is a non-breaking change
(see [`ARCHITECTURE.md`](./ARCHITECTURE.md) for the four-laws statement).

`metacore-kernel` consumes the public [`metacore-sdk`](https://github.com/asteby/metacore-sdk)
for manifest, bundle and dynamic-table type definitions; it does **not**
publish addons of its own. Addons are authored against the SDK CLI
(`metacore build` / `compile-wasm`) and executed by this kernel.

## Dynamic CRUD framework

> **Zero-glue CRUD.** Declare a model in `manifest.json`, install the
> addon, get a working API and admin UI. No handlers, no migrations, no
> per-model React.

When an addon ships `model_definitions[]` in its manifest, `installer.Install`
creates the Postgres schema, runs DDL, registers metadata, and the kernel's
`dynamic.Handler` starts serving live CRUD endpoints — all without addon code:

```json
{
  "key": "tickets",
  "model_definitions": [{
    "table_name": "tickets",
    "org_scoped": true,
    "soft_delete": true,
    "columns": [
      { "name": "subject",  "type": "string", "size": 200, "required": true, "index": true },
      { "name": "status",   "type": "string", "size": 24,  "default": "'open'" },
      { "name": "priority", "type": "string", "size": 12 },
      { "name": "due_at",   "type": "timestamp" }
    ]
  }],
  "capabilities": [
    { "kind": "db:read",  "target": "addon_tickets.*" },
    { "kind": "db:write", "target": "addon_tickets.*" }
  ]
}
```

That manifest is the **only** code needed to expose:

| Endpoint                                | Behaviour                                            |
| --------------------------------------- | ---------------------------------------------------- |
| `GET/POST /api/dynamic/tickets`         | Paginated list, filters, sort, search · create        |
| `GET/PUT/DELETE /api/dynamic/tickets/:id` | Get, update (load-merge-save), soft delete           |
| `GET /api/options/tickets`              | Select/lookup options (with resolver)                |
| `GET /api/search/tickets`               | Full-text search over searchable columns             |
| `GET /api/metadata/table/tickets`       | TableMetadata for the runtime-react `DynamicTable`   |
| `GET /api/metadata/modal/tickets`       | ModalMetadata for the runtime-react form generator   |

Per-request user capabilities (`tickets.read`, `tickets.create`, …) are
gated by `permission.Service`; addon-level capabilities (`db:write
addon_tickets.*`) are gated by `security.Enforcer` in either shadow or
enforce mode.

Read [`docs/dynamic-system.md`](./docs/dynamic-system.md) for the full
end-to-end walkthrough,
[`docs/dynamic-api.md`](./docs/dynamic-api.md) for the HTTP reference, and
[`docs/permissions.md`](./docs/permissions.md) for the capability model.

## Architecture

```
                    ┌──────────────────────────────────────────────┐
                    │                Host application              │
                    │            (link, ops, pilot, …)             │
                    │                                              │
                    │  fiber.App  ──►  host.App.Mount(/api)        │
                    └──────┬───────────────────────────────────────┘
                           │ embeds via go.mod
                           ▼
                    ┌──────────────────────────────────────────────┐
                    │            metacore-kernel (this repo)       │
                    │                                              │
                    │  ┌─────────────┐   ┌─────────────────────┐   │
                    │  │   host/     │──►│   auth · permission │   │
                    │  │ App + Host  │   │   metadata · dynamic│   │
                    │  │  facades    │   │   query · obs       │   │
                    │  └──────┬──────┘   └─────────────────────┘   │
                    │         │                                    │
                    │  ┌──────▼──────┐   ┌─────────────────────┐   │
                    │  │ lifecycle/  │──►│    events/  (bus)   │   │
                    │  │ installer/  │   │    eventlog/ (log)  │   │
                    │  │ navigation/ │   │    ws/  push/       │   │
                    │  │ manifest/   │   │    notifications/   │   │
                    │  └──────┬──────┘   │    webhooks/        │   │
                    │         │          └─────────────────────┘   │
                    │  ┌──────▼──────┐   ┌─────────────────────┐   │
                    │  │ runtime/    │──►│    security/        │   │
                    │  │   wasm/     │   │  Enforcer · HMAC    │   │
                    │  │  (wazero)   │   │  Capabilities       │   │
                    │  └──────┬──────┘   └─────────────────────┘   │
                    └─────────┼────────────────────────────────────┘
                              │ host module imports (gated)
                              ▼
                    ┌──────────────────────────────────────────────┐
                    │              Addon WASM module               │
                    │   built with metacore-sdk CLI · sandboxed    │
                    │   exports: alloc(i32) · <fn>(ptr,len) i64    │
                    └──────────────────────────────────────────────┘
```

### Dynamic CRUD flow

```
manifest.json (model_definitions[], capabilities[])
        │
        ▼
installer.Install
   ├──► dynamic.EnsureSchema       (CREATE SCHEMA addon_<key>)
   ├──► dynamic.Apply              (versioned SQL migrations)
   ├──► dynamic.CreateTable        (CREATE TABLE + RLS policy)
   ├──► dynamic.SyncSchema         (ADD COLUMN IF NOT EXISTS)
   └──► lifecycle OnInstall/OnEnable
        │
        ▼
modelbase.Register("<model>", factory)        ◄── host wires this
        │
        ▼
host.App.Mount
   ├──► metadata.Handler  ──►  GET /metadata/{table,modal,all}/:model
   └──► dynamic.Handler   ──►  GET/POST/PUT/DELETE /dynamic/:model
                                GET /options/:model · /search/:model
        │                       ▲
        │                       │ permission.Service.Check (per request)
        ▼
@asteby/metacore-runtime-react renders the table + modal from metadata
```

Two transports talk to the frontend at the same time:

1. **WebSocket** (`ws/`) — bidirectional, primary channel for live updates,
   notifications, addon events.
2. **REST** — Fiber routes mounted under `/api/*` for auth, metadata, CRUD,
   navigation, push subscriptions, webhooks, addon installs.

Every privileged operation an addon attempts (DB read, event publish, HTTP
fetch, …) is brokered by the `security.Enforcer`. The enforcer can run in
`shadow` mode (log only) during rollout and is flipped to `enforce` via the
`METACORE_ENFORCE` environment variable without redeploys.

## Embedding

The kernel exposes two facades. Pick the highest-level one that fits.

### Option 1 — `host.App` (recommended)

`host.App` wires `auth + metadata + dynamic CRUD + WebSocket hub` and,
optionally, `permission`, `push`, `webhooks` and Prometheus metrics. It is the
single call most apps need.

```go
package main

import (
    "log"
    "os"

    "github.com/gofiber/fiber/v2"
    "gorm.io/driver/postgres"
    "gorm.io/gorm"

    "github.com/asteby/metacore-kernel/host"
    "github.com/asteby/metacore-kernel/modelbase"
)

func main() {
    db, err := gorm.Open(postgres.Open(os.Getenv("DATABASE_URL")), &gorm.Config{})
    if err != nil {
        log.Fatalf("db: %v", err)
    }

    app := host.NewApp(host.AppConfig{
        DB:             db,
        JWTSecret:      []byte(os.Getenv("JWT_SECRET")),
        RunMigrations:  true,
        EnableMetrics:  true,
        EnableWebhooks: true,
    }).RegisterModel("products", func() modelbase.ModelDefiner {
        return &Product{}
    })

    fiberApp := fiber.New()
    app.Mount(fiberApp.Group("/api"))

    log.Fatal(fiberApp.Listen(":3000"))
}
```

`Mount` returns the authenticated sub-router, so the app can layer its own
domain handlers on top of the kernel-provided ones.

### Option 2 — `host.Host` (addon-platform mode)

When the binary needs to **host WASM addons** (install, enable, lifecycle
hooks, navigation merge), construct a `host.Host` instead. This is what `link`
uses to load conversational addons; `ops` uses it for marketplace integrations.

```go
h, err := host.New(host.Config{
    DB:            db,
    KernelVersion: "0.2.0",
    Services: map[string]any{
        "eventbus": bus,
    },
})
if err != nil { log.Fatal(err) }

h.RegisterCompiled("billing", &billing.Addon{})

if err := h.Boot(); err != nil { log.Fatal(err) }
```

The two facades compose: real apps build a `host.App` for HTTP plumbing and a
`host.Host` for the addon plane, sharing the same `*gorm.DB`.

For private-module access (`GOPRIVATE`, SSH/netrc) and a step-by-step
walk-through, see [`docs/CONSUMER_GUIDE.md`](./docs/CONSUMER_GUIDE.md).

## Subsystems

| Package           | Responsibility                                                                |
| ----------------- | ----------------------------------------------------------------------------- |
| `host/`           | `App` and `Host` facades — boot orchestration and Fiber mount                 |
| `auth/`           | JWT issue/verify, password hashing, login/refresh handlers, Fiber middleware  |
| `permission/`     | Role + capability checks; pluggable `PermissionStore`                         |
| `metadata/`       | `TableMetadata` / `ModalMetadata` registry + cache + handler                  |
| `dynamic/`        | Generic CRUD over registered models, options/search resolvers                 |
| `query/`          | Filter/sort/paginate query builder                                            |
| `modelbase/`      | Stable interfaces (`AuthUser`, `AuthOrg`, `ModelDefiner`) + base structs      |
| `obs/`            | Structured `slog` logger with request-id propagation                          |
| `ws/`             | WebSocket hub, per-user routing, broadcast helpers                            |
| `push/`           | Web Push (VAPID) subscriptions and dispatch                                   |
| `webhooks/`       | Outbound HMAC-signed webhooks with retry queue                                |
| `notifications/`  | Delivery queue, dedup, retry, pluggable `ChannelHandler`                      |
| `eventlog/`       | Org-scoped persisted event log with cursor pagination                         |
| `events/`         | In-process pub/sub bus for addons (capability-checked, wildcard patterns)     |
| `lifecycle/`      | Addon contract (`Manifest`, `OnInstall`, …) + registry + interceptors         |
| `installer/`      | Install / enable / disable / uninstall flow + frontend bundle materialization |
| `navigation/`     | Merge core sidebar groups with addon contributions                            |
| `manifest/`       | Declarative addon manifest schema (mirrored by SDK)                           |
| `bundle/`         | Addon bundle I/O contracts (`bundle.tgz` reader/writer)                       |
| `tool/`           | Addon tool runtime + dispatcher + registry                                    |
| `bridge/`         | Adapters that map kernel actions/tools/webhooks to host integrations          |
| `runtime/wasm/`   | wazero-based WASM runtime, ABI, capability-gated host imports                 |
| `security/`       | `Enforcer`, `Capabilities`, HMAC, secretbox, nonce store, webhook dispatch    |
| `metrics/`        | Prometheus registry, Fiber middleware, `/metrics` handler                    |
| `migrations/`     | Versioned SQL migration runner (Goose) for kernel-owned tables                |
| `httpx/`          | HTTP helpers reused across handlers                                           |
| `log/`            | Builder-style logger (legacy; new code uses `obs/`)                           |

A package-by-package contract — what belongs in the kernel versus the SDK
versus an app — is documented in [`ARCHITECTURE.md`](./ARCHITECTURE.md).

## WebSocket protocol

Clients connect to `wss://<host>/api/ws?token=<jwt>`. The hub propagates the
authenticated `user_id` from `Locals` into the connection and registers a
per-user fan-out. Messages are JSON envelopes:

```json
{ "type": "NOTIFICATION", "payload": { /* … */ } }
```

Standard message types live in `ws/hub.go` (`MsgNotification`,
`MsgStatusUpdate`, `MsgCustom`). Apps can declare their own constants — the
type is a plain string, no fork required.

For org-wide broadcasts, callers query their own DB for user IDs and call
`hub.SendToUsers(ids, msg)`. Notification persistence is delegated to the
optional `OnNotification` hook so the hub stays ORM-free.

## Addon contract

Addons are **authored against [`metacore-sdk`](https://github.com/asteby/metacore-sdk)**
and **executed by this kernel**. The split is intentional:

- **SDK** (public, npm + Go CLI) owns the developer experience: `metacore init`,
  `metacore validate`, `metacore build`, `metacore sign`, `metacore compile-wasm`,
  the manifest schema, the bundle layout, the TypeScript types for runtime
  React.
- **Kernel** (this repo, private) owns the runtime: parses the manifest,
  verifies signatures, materialises bundles on disk, instantiates the WASM
  module, brokers every host import, runs lifecycle hooks, merges navigation,
  exposes installer endpoints.

The wire-level WASM ABI is defined in `runtime/wasm/abi.go` and is documented
on the SDK side at `docs/wasm-abi.md` (single source of truth — the kernel is
the implementer, the SDK is the reference).

## Documentation

| Document                                                         | Audience                                              |
| ---------------------------------------------------------------- | ----------------------------------------------------- |
| [`ARCHITECTURE.md`](./ARCHITECTURE.md)                           | Maintainers — the four laws of the kernel             |
| [`docs/dynamic-system.md`](./docs/dynamic-system.md)             | App teams using the dynamic CRUD framework            |
| [`docs/dynamic-api.md`](./docs/dynamic-api.md)                   | Anyone calling the dynamic / metadata HTTP endpoints  |
| [`docs/permissions.md`](./docs/permissions.md)                   | Auth / capability model — user gates and addon gates  |
| [`docs/embedding-quickstart.md`](./docs/embedding-quickstart.md) | First-time hosts — 10-minute walkthrough              |
| [`docs/CONSUMER_GUIDE.md`](./docs/CONSUMER_GUIDE.md)             | App teams embedding the kernel (long form)            |
| [`docs/dev-setup.md`](./docs/dev-setup.md)                       | Contributors working on the kernel itself             |
| [`docs/RELEASE.md`](./docs/RELEASE.md)                           | Release manager and consumers                         |
| [`docs/consumer-renovate-template.json`](./docs/consumer-renovate-template.json) | Drop-in Renovate config for consumers     |
| [`CHANGELOG.md`](./CHANGELOG.md)                                 | Anyone consuming a new tag                            |

## Development

```bash
git clone git@github.com:asteby/metacore-kernel.git
cd metacore-kernel

# Tests with the race detector — same flags CI runs.
go test -race ./...

# Static analysis.
go vet ./...

# Single package, verbose.
go test -race -v ./runtime/wasm/...
```

Local development assumes the SDK is checked out as a sibling directory:

```
~/projects/metacore-sdk
~/projects/metacore-kernel
```

`go.mod` carries a `replace github.com/asteby/metacore-sdk => ../metacore-sdk`
directive so SDK changes are picked up without publishing a tag. Drop the
replace before tagging a release — see [`docs/dev-setup.md`](./docs/dev-setup.md).

## Release process

Releases are tag-driven. `git push origin vX.Y.Z` triggers
`.github/workflows/release.yml`, which runs the test suite, pings the Go
proxy, publishes a GitHub Release via GoReleaser, and dispatches a
`metacore-kernel-released` event to every consumer repository so Renovate runs
on demand instead of waiting for the next cron tick.

The full procedure — version selection, pre-releases, retract, troubleshooting
— is in [`docs/RELEASE.md`](./docs/RELEASE.md).

## Module path and private access

```
github.com/asteby/metacore-kernel
```

The repository is private. Configure Go to skip the public module proxy on
every developer and CI machine:

```bash
go env -w GOPRIVATE="github.com/asteby/*"
```

For SSH-authenticated developers:

```bash
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

For headless / CI:

```bash
cat > ~/.netrc <<EOF
machine github.com
  login x-access-token
  password $GITHUB_TOKEN
EOF
chmod 600 ~/.netrc
```

Both flows are documented in [`docs/CONSUMER_GUIDE.md`](./docs/CONSUMER_GUIDE.md).

## License

Proprietary — © Asteby. All rights reserved.

This module is distributed under a closed-source license to repositories
within the `asteby/*` GitHub organization. External redistribution is not
permitted without prior written authorization.
