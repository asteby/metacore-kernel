# Consumer Guide

This guide is for engineers integrating `metacore-kernel` into a Go backend.
It assumes you are building a host application that embeds the kernel.
Frontend addon authors should read the
[`metacore-sdk`](https://github.com/asteby/metacore-sdk) documentation
instead — this kernel only executes what the SDK produces.

---

## Table of contents

1. [Installing the module](#1-installing-the-module)
2. [Private-module access](#2-private-module-access)
3. [Quickstart — `host.App`](#3-quickstart--hostapp)
4. [Adding the addon plane — `host.Host`](#4-adding-the-addon-plane--hosthost)
5. [Storage and migrations](#5-storage-and-migrations)
6. [Capability model and security modes](#6-capability-model-and-security-modes)
7. [WebSocket hub](#7-websocket-hub)
8. [Real-time updates](#8-real-time-updates)
9. [Renovate template](#9-renovate-template)
10. [SemVer policy](#10-semver-policy)
11. [End-to-end release flow](#11-end-to-end-release-flow)
12. [FAQ](#12-faq)

> Looking for a single-page walkthrough? Try
> [`embedding-quickstart.md`](embedding-quickstart.md). Looking for the dynamic
> CRUD framework spec? See [`dynamic-system.md`](dynamic-system.md). For
> permission details, [`permissions.md`](permissions.md).

---

## 1. Installing the module

```bash
go get github.com/asteby/metacore-kernel@latest
go mod tidy
```

Pin a specific tag in production:

```bash
go get github.com/asteby/metacore-kernel@v0.2.0
```

Once the module is in your `go.mod`:

```go
require github.com/asteby/metacore-kernel v0.2.0
```

For local development against an in-progress kernel, drop a `replace`
directive into your app's `go.mod`:

```go
replace github.com/asteby/metacore-kernel => ../metacore-kernel
```

Run `go mod edit -dropreplace github.com/asteby/metacore-kernel` and `go mod
tidy` before you commit, so production builds resolve to a tagged version.

## 2. Private-module access

The kernel lives in a private repository. Configure each developer machine
and CI runner once.

### Environment

```bash
go env -w GOPRIVATE="github.com/asteby/*"
go env -w GOSUMDB=off                            # private modules skip sumdb
```

Per-shell equivalent:

```bash
export GOPRIVATE="github.com/asteby/*"
export GOSUMDB=off
```

### SSH (developers)

```bash
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

Requires an SSH key registered with GitHub
(`ssh-keygen -t ed25519 -C "you@example.com"` and add the `.pub` at
[github.com/settings/keys](https://github.com/settings/keys)).

### Token (CI / headless)

```bash
cat > ~/.netrc <<EOF
machine github.com
  login x-access-token
  password ${GITHUB_TOKEN}
EOF
chmod 600 ~/.netrc
```

In GitHub Actions for consumer repositories, mint a fine-grained token with
read access to `asteby/metacore-kernel` and bind it before `go mod download`:

```yaml
- name: Configure netrc
  run: |
    cat > ~/.netrc <<EOF
    machine github.com
      login x-access-token
      password ${{ secrets.METACORE_READ_TOKEN }}
    EOF
    chmod 600 ~/.netrc
```

## 3. Quickstart — `host.App`

`host.App` is the recommended entry point. It wires `auth + metadata + dynamic
CRUD + WebSocket hub` and, when enabled, `permission`, `push`, `webhooks` and
Prometheus metrics. The minimal embedder is two screens long:

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

type Product struct {
    modelbase.BaseUUIDModel
    Name  string  `gorm:"size:120;not null" json:"name"`
    Price float64 `json:"price"`
}

// modelbase.ModelDefiner is the contract dynamic / metadata use to introspect
// a model. It has three methods — TableName, DefineTable, DefineModal.
func (Product) TableName() string { return "products" }

func (Product) DefineTable() modelbase.TableMetadata {
    return modelbase.TableMetadata{
        Title: "Products",
        Columns: []modelbase.ColumnDef{
            {Key: "name",  Label: "Name",  Type: "text",   Sortable: true},
            {Key: "price", Label: "Price", Type: "number", Sortable: true},
        },
        SearchColumns:     []string{"name"},
        EnableCRUDActions: true,
    }
}

func (Product) DefineModal() modelbase.ModalMetadata {
    return modelbase.ModalMetadata{
        Title: "Product",
        Fields: []modelbase.FieldDef{
            {Key: "name",  Label: "Name",  Type: "text",   Required: true},
            {Key: "price", Label: "Price", Type: "number"},
        },
    }
}

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
    defer app.Stop()

    fiberApp := fiber.New()

    // app.Mount returns the authenticated sub-router so apps can append
    // their own domain routes on top of the kernel-provided ones.
    api := app.Mount(fiberApp.Group("/api"))
    api.Get("/me", func(c *fiber.Ctx) error { /* … */ return nil })

    log.Fatal(fiberApp.Listen(":3000"))
}
```

`App.RegisterModel(key, factory)` ([`host/app.go`](../host/app.go)) wires a
factory into the kernel registry. The factory MUST return a fresh,
zero-valued instance on every call — `dynamic.Service` instantiates one per
request and mutates it. The returned value MUST satisfy
`modelbase.ModelDefiner`:

```go
type ModelDefiner interface {
    TableName() string
    DefineTable() TableMetadata
    DefineModal() ModalMetadata
}
```

`TableName` selects the database table (must match the table the kernel
created — see [`dynamic-system.md`](dynamic-system.md) for declarative
addons whose tables are produced by the installer). `DefineTable` and
`DefineModal` drive the metadata endpoints and, by extension, the
runtime-react UI. Any change to the JSON tags on `TableMetadata` /
`ModalMetadata` is a MAJOR version bump — they are part of the wire
contract.

What you get for free:

| Mount point                | Source        | Notes                                                  |
| -------------------------- | ------------- | ------------------------------------------------------ |
| `POST /api/auth/login`     | `auth/`       | JWT issuance, password verification                    |
| `POST /api/auth/refresh`   | `auth/`       | Rotate access token                                    |
| `GET  /api/metadata/:name` | `metadata/`   | Cached `TableMetadata` / `ModalMetadata`               |
| CRUD `GET/POST/PUT/DELETE` | `dynamic/`    | Generic over every registered model                    |
| `GET  /api/push/*`         | `push/`       | Web Push (when `EnablePush=true`)                      |
| `GET  /api/webhooks/*`     | `webhooks/`   | When `EnableWebhooks=true`                             |
| `GET  /api/ws?token=…`     | `ws/`         | WebSocket upgrade                                      |
| `GET  /metrics`            | `metrics/`    | Prometheus exposition (`EnableMetrics=true`)           |

## 4. Adding the addon plane — `host.Host`

If your app should host federated WASM addons (install/enable/disable,
lifecycle hooks, navigation merge), build a `host.Host` next to the
`host.App`. Both share the same `*gorm.DB`.

```go
import (
    "github.com/asteby/metacore-kernel/host"
    "github.com/asteby/metacore-kernel/lifecycle"
)

h, err := host.New(host.Config{
    DB:            db,
    KernelVersion: "0.2.0",
    Services: map[string]any{
        "eventbus": bus,
        "fiscal":   fiscalSvc,
    },
})
if err != nil {
    log.Fatal(err)
}

// Compiled-in addons (Go code linked into the host binary)
h.RegisterCompiled("billing", &billing.Addon{})

// Run every addon's Boot() hook with the shared services.
if err := h.Boot(); err != nil {
    log.Fatal(err)
}

// Render the merged sidebar for an organization.
groups, err := h.Navigation(orgID, coreGroups)
```

Addon types:

- **Compiled** — Go code linked into the host. Highest trust, fastest
  invocation; registered via `RegisterCompiled`.
- **Declarative** — manifest-only. Behavior wired through webhooks and
  interceptors registered at `Boot()`.
- **Federated WASM** — `bundle.tgz` produced by `metacore-sdk`,
  installed via `installer.Installer`. The kernel verifies the manifest
  signature, materialises any frontend assets under `FrontendBasePath`, and
  hands the WASM module to `runtime/wasm.Host` for execution under the
  capability enforcer.

## 5. Storage and migrations

The kernel ships **versioned SQL migrations** for its own tables (`auth`,
`webhooks`, `push`, `installer`, `eventlog`, `notifications`).

```go
host.NewApp(host.AppConfig{
    DB:            db,
    JWTSecret:     secret,
    RunMigrations: true, // recommended for production
})
```

`RunMigrations: true` invokes `migrations.Runner` (Goose-based, tracks state
in `goose_db_version`). Setting it to `false` falls back to GORM `AutoMigrate`
for the same set of tables — convenient locally, but unsafe across kernel
upgrades. Treat AutoMigrate as a development-only path.

PostgreSQL is the supported production driver. The kernel also tests against
SQLite (`gorm.io/driver/sqlite`) for embedded scenarios; mileage on dialect-
specific features may vary.

## 6. Capability model and security modes

Every addon-issued operation that touches the host (DB read, event publish,
HTTP call out) goes through `security.Enforcer`. The enforcer has two modes:

- `ModeShadow` — log violations, never block. Default during rollout.
- `ModeEnforce` — return an error on violations.

Operators flip the mode at runtime via the `METACORE_ENFORCE` environment
variable (`1`, `true`, `yes` enable enforce). No redeploy required.

```go
enf := security.NewEnforcer(security.ModeFromEnv())
```

Capabilities are declared per addon in its manifest and resolved into a
compiled `Capabilities` set at install time. Examples:

| Capability         | Granted to                                      |
| ------------------ | ----------------------------------------------- |
| `event:emit`       | Addons that need to publish on the in-process bus |
| `event:subscribe`  | Addons that consume events (wildcard supported) |
| `db:read`          | Read access through the dynamic CRUD service    |
| `http:fetch`       | Outbound HTTP from inside the WASM sandbox      |

Violations are reported via the kernel's structured logger; in shadow mode
they appear as `level=warn category=enforcer mode=shadow` so operators can
audit usage before flipping to enforce.

The complete list of capabilities and the format of the manifest section that
declares them lives in the SDK documentation (`docs/manifest.md`).

The kernel also ships a **user-level** capability system
(`permission.Service`) that gates every dynamic CRUD request on
`<resource>.<action>` capabilities. Wire `host.AppConfig.PermissionStore` to
turn it on. See [`permissions.md`](permissions.md) for the full model
(stores, super-roles, Fiber gate middleware, addon vs user gates).

## 7. WebSocket hub

The hub is mounted automatically by `host.App.Mount` at `/api/ws`. Auth is
JWT-based, taken from the `?token=` query string at upgrade time:

```
wss://api.example.com/api/ws?token=<jwt>
```

Send messages from your domain code:

```go
app.WSHub.SendToUsers(userIDs, ws.Message{
    Type:    ws.MsgNotification,
    Payload: payload,
})
```

`MessageType` is a plain string; declare your own constants in app code
without forking the package. The hub does not persist anything — wire the
optional `OnNotification` hook if your app needs durable storage.

## 8. Real-time updates

The dynamic CRUD layer **does not** broadcast row changes automatically.
The kernel ships the hub; the host decides who receives a message. The
recommended pattern is to wrap the dynamic service so every mutation
publishes a typed message to the affected users:

```go
import (
    "context"

    "github.com/asteby/metacore-kernel/dynamic"
    "github.com/asteby/metacore-kernel/modelbase"
    "github.com/asteby/metacore-kernel/ws"
    "github.com/google/uuid"
)

const MsgTicketCreated ws.MessageType = "TICKET_CREATED"

type ticketRealtime struct {
    dyn  *dynamic.Service
    hub  *ws.Hub
    orgUserIDs func(context.Context, uuid.UUID) []uuid.UUID
}

func (t *ticketRealtime) Create(ctx context.Context, user modelbase.AuthUser, in map[string]any) (map[string]any, error) {
    out, err := t.dyn.Create(ctx, "tickets", user, in)
    if err != nil {
        return nil, err
    }
    t.hub.SendToUsers(
        t.orgUserIDs(ctx, user.GetOrganizationID()),
        ws.Message{Type: MsgTicketCreated, Payload: out},
    )
    return out, nil
}
```

`Hub.SendToUsers` ([`ws/hub.go`](../ws/hub.go)) is fire-and-forget,
non-blocking, and per-process. For multi-replica deployments, fan out via
the addon event bus ([`events/`](../events/)) and have each replica
subscribe to a forwarder that re-publishes to its local hub — the hub is
a process-local primitive on purpose.

For per-model hooks, register on a `dynamic.HookRegistry` and pass it into
`dynamic.Config.Hooks` (the registry is keyed by model name):

```go
hooks := dynamic.NewHookRegistry()
hooks.RegisterAfterCreate("tickets", func(ctx context.Context, hc dynamic.HookContext, record any) error {
    hub.SendToUsers(
        orgUserIDs(ctx, hc.User.GetOrganizationID()),
        ws.Message{Type: MsgTicketCreated, Payload: record},
    )
    return nil
})
```

See [`dynamic-system.md`](dynamic-system.md), section *Real-time updates*,
for the rationale and trade-offs.

## 9. Renovate template

Copy [`docs/consumer-renovate-template.json`](./consumer-renovate-template.json)
to the root of your consumer repository as `renovate.json`. The template
encodes the policy the ecosystem agreed on:

```json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": ["config:recommended", ":semanticCommits"],
  "schedule": ["before 6am on monday"],
  "packageRules": [
    {
      "matchManagers": ["gomod"],
      "matchPackagePatterns": ["^github.com/asteby/metacore-kernel"],
      "matchUpdateTypes": ["patch", "minor"],
      "automerge": true,
      "platformAutomerge": true,
      "groupName": "metacore-kernel"
    },
    {
      "matchManagers": ["gomod"],
      "matchPackagePatterns": ["^github.com/asteby/metacore-kernel"],
      "matchUpdateTypes": ["major"],
      "automerge": false,
      "labels": ["breaking", "review-required"]
    }
  ]
}
```

Prerequisites in the consumer repository:

1. **Renovate GitHub App** installed with access to the repo.
2. **Allow auto-merge** in Settings → General (enables `platformAutomerge`).
3. **Branch protection** on `main` requiring CI to pass before merge.
4. A token with `repo:read` on `asteby/metacore-kernel`, exposed to Renovate
   via `hostRules` (Renovate Cloud) or `secrets.RENOVATE_GITHUB_TOKEN`
   (self-hosted).

### Update cadence

The kernel does not push notifications to consumers. Renovate / Dependabot
poll the Go proxy on their own schedule (Renovate default: hourly) and open
a PR when a new tag is indexed. If you want faster pickup, lower the
`schedule` in `renovate.json` or run a manual rerun from the Renovate
dashboard after cutting a kernel release.

## 10. SemVer policy

The kernel follows [SemVer 2.0](https://semver.org/) strictly. When Renovate
opens a bump PR, read the version delta:

| Bump                           | Meaning                                                 | Default action     |
| ------------------------------ | ------------------------------------------------------- | ------------------ |
| `vX.Y.Z` → `vX.Y.(Z+1)`        | Patch — bug fixes only                                  | Auto-merge on green CI |
| `vX.Y.Z` → `vX.(Y+1).0`        | Minor — new symbols, backward-compatible                | Auto-merge if your CI exercises kernel routes |
| `vX.Y.Z` → `v(X+1).0.0`        | Major — breaking API changes; import path changes (`/v2`) | Manual review required |

What we never do: silently change the meaning of an exported symbol within
the same major. Adding a method to an interface, removing a field from a
public struct, or changing a function signature is always a major bump (see
`ARCHITECTURE.md`, *Semver discipline*).

### Risk signals on a Renovate PR

- **CI fails on the consumer** — do not merge; open an upstream issue.
- **Changelog mentions schema change** — verify your migration runner is
  configured (`RunMigrations: true`).
- **Pre-1.0 minor (`v0.5` → `v0.6`)** — treat as potentially breaking even
  though it is technically minor; `v0.x` releases retain the right to break.

## 11. End-to-end release flow

```
[Kernel] git tag vX.Y.Z && git push --tags
       │
       ▼
[Kernel] Release workflow: tests → proxy ping → GoReleaser
       │
       ▼
[Go proxy] indexes the new tag (`proxy.golang.org`)
       │
       ▼
[Consumer] Renovate / Dependabot polls the proxy → detects new version
       │
       ▼
[Consumer] PR "chore(deps): update github.com/asteby/metacore-kernel to vX.Y.Z"
       │
       ▼
[Consumer] CI green → Renovate auto-merge → main updated
       │
       ▼
[Consumer] Deploy pipeline (out of scope for this repo)
```

End-to-end latency is typically 5–15 minutes from `git push --tags` to every
consumer's `main`.

## 12. FAQ

**Can I bypass the Go proxy?**
Yes. `GOPROXY=direct go get github.com/asteby/metacore-kernel@<branch-or-sha>`
fetches straight from GitHub. Useful for testing un-tagged work.

**How do I pin to a specific commit?**
`go get github.com/asteby/metacore-kernel@<sha>` resolves to a pseudo-version
(`v0.0.0-YYYYMMDDhhmmss-<sha12>`) — fine for development branches, do not
use in production releases.

**Can I fork the kernel?**
Forking breaks Renovate for your consumer (you stop receiving upstream bumps)
and forks your security model. Open an issue or a PR upstream instead.

**Where is the WASM ABI documented?**
Single source of truth lives in the SDK at `docs/wasm-abi.md`. The
implementation is `runtime/wasm/abi.go` in this repo.

**My handler imports `fiber`. Is the kernel framework-locked?**
Services (`*.Service` types) are framework-agnostic and accept
`context.Context`. Handlers (`*.Handler`) are Fiber-specific by convention.
If you switch transports (gRPC, Echo, Lambda), consume the services directly
and write your own handler — see `ARCHITECTURE.md`, *Law 3*.
