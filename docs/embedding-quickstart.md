<p align="center">
  <img src="assets/metacore.svg" width="120" alt="Metacore Kernel" />
</p>

<h1 align="center">Embedding Quickstart</h1>

<p align="center"><em>Your first host with the kernel embedded — in 10 minutes.</em></p>

---

## Table of contents

- [Goal](#goal)
- [Prerequisites](#prerequisites)
- [1. New Go module](#1-new-go-module)
- [2. Wire main.go](#2-wire-maingo)
- [3. Storage and migrations](#3-storage-and-migrations)
- [4. Boot the addon plane](#4-boot-the-addon-plane)
- [5. Install your first addon](#5-install-your-first-addon)
- [6. Verify the dynamic CRUD endpoints](#6-verify-the-dynamic-crud-endpoints)
- [7. Pair with a frontend](#7-pair-with-a-frontend)
- [Next steps](#next-steps)

---

## Goal

Stand up a Fiber-based HTTP server that:

- exposes `auth + metadata + dynamic CRUD + WebSocket hub` (via `host.App`),
- runs the addon lifecycle plane (via `host.Host`),
- accepts a sample addon bundle and turns its `model_definitions[]` into
  live CRUD endpoints,
- enforces user capabilities and addon capabilities.

If you just want the dynamic CRUD layer without the addon plane, skip
section 4.

## Prerequisites

| Tool      | Version          |
| --------- | ---------------- |
| Go        | 1.25+            |
| Postgres  | 14+              |
| GitHub access | `GOPRIVATE="github.com/asteby/*"` configured (see [`CONSUMER_GUIDE.md`](CONSUMER_GUIDE.md#2-private-module-access)) |

```bash
go env -w GOPRIVATE="github.com/asteby/*"
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

## 1. New Go module

```bash
mkdir my-host && cd my-host
go mod init example.com/my-host
go get github.com/asteby/metacore-kernel@latest
go get github.com/gofiber/fiber/v2 gorm.io/gorm gorm.io/driver/postgres github.com/google/uuid
```

## 2. Wire main.go

```go
package main

import (
    "log"
    "os"

    "github.com/gofiber/fiber/v2"
    "gorm.io/driver/postgres"
    "gorm.io/gorm"

    "github.com/asteby/metacore-kernel/host"
    "github.com/asteby/metacore-kernel/permission"
)

func main() {
    db, err := gorm.Open(
        postgres.Open(os.Getenv("DATABASE_URL")),
        &gorm.Config{},
    )
    if err != nil {
        log.Fatalf("db: %v", err)
    }

    // GORM-backed permission store. Production default.
    permStore, err := permission.NewGormStore(db)
    if err != nil {
        log.Fatalf("permission store: %v", err)
    }

    app := host.NewApp(host.AppConfig{
        DB:              db,
        JWTSecret:       []byte(host.MustGetenv("JWT_SECRET")),
        RunMigrations:   true,            // versioned SQL via migrations.Runner
        EnableMetrics:   true,            // exposes /api/metrics
        EnableWebhooks:  true,
        PermissionStore: permStore,       // turn on user-level CRUD gates
    })
    defer app.Stop()

    fiberApp := fiber.New()
    api := app.Mount(fiberApp.Group("/api"))

    // Layer your own domain endpoints on top of the kernel's.
    api.Get("/me", whoAmI)

    log.Fatal(fiberApp.Listen(":3000"))
}

func whoAmI(c *fiber.Ctx) error {
    return c.JSON(fiber.Map{"ok": true})
}
```

What this gets you for free, without writing a single handler:

| Mount point                               | Source        |
| ----------------------------------------- | ------------- |
| `POST /api/auth/login`                    | [`auth/`](../auth/)         |
| `POST /api/auth/refresh`                  | [`auth/`](../auth/)         |
| `GET  /api/metadata/table/:model`         | [`metadata/`](../metadata/) |
| `GET  /api/metadata/modal/:model`         | [`metadata/`](../metadata/) |
| `GET  /api/metadata/all`                  | [`metadata/`](../metadata/) |
| `GET/POST/PUT/DELETE /api/dynamic/:model` | [`dynamic/`](../dynamic/) (auto-mounted) |
| `GET  /api/options/:model`                | [`dynamic/`](../dynamic/) (host calls `MountOptions` to enable) |
| `GET  /api/search/:model`                 | [`dynamic/`](../dynamic/) (host calls `MountOptions` to enable) |
| `GET  /api/webhooks/*`                    | [`webhooks/`](../webhooks/) |
| `GET  /api/ws?token=…`                    | [`ws/`](../ws/)             |
| `GET  /api/metrics`                       | [`metrics/`](../metrics/) (mounted on the same router passed to `Mount`) |

The full route list and configuration knobs are in
[`host/app.go`](../host/app.go).

## 3. Storage and migrations

`RunMigrations: true` invokes the Goose-based runner
([`migrations/runner.go`](../migrations/)) on every boot — idempotent,
state tracked in the `goose_db_version` table. This is the recommended
production path.

Setting it to `false` falls back to GORM `AutoMigrate` for the kernel's
own tables — fine locally, unsafe across kernel upgrades.

PostgreSQL is the supported production driver; SQLite is only used in tests.

## 4. Boot the addon plane

If your host should accept addon bundles (install / enable / disable /
uninstall, lifecycle hooks, navigation merge, dynamic schema), build a
`host.Host` next to the `host.App`. They share the same `*gorm.DB`.

```go
import "github.com/asteby/metacore-kernel/host"

h, err := host.New(host.Config{
    DB:            db,
    KernelVersion: "0.7.2",
    Services: map[string]any{
        // Anything addon Boot() hooks need.
        // "eventbus": eventBus,
    },
})
if err != nil {
    log.Fatalf("host.New: %v", err)
}

if err := h.Boot(); err != nil {
    log.Fatalf("Boot: %v", err)
}
```

`host.Host` ([`host/host.go`](../host/host.go)) owns the `Installer`,
`Lifecycles`, and `Interceptors`. Compiled-in addons register before
`Boot`:

```go
h.RegisterCompiled("billing", &billing.Addon{})
```

## 5. Install your first addon

Read a `tickets.tgz` bundle (produced by `metacore build`) from disk and
hand it to the installer:

```go
import (
    "os"

    "github.com/asteby/metacore-kernel/bundle"
    "github.com/google/uuid"
)

f, err := os.Open("/var/addons/tickets.tgz")
if err != nil {
    log.Fatalf("open bundle: %v", err)
}
defer f.Close()

b, err := bundle.Read(f, 64<<20) // 64 MiB max decompressed
if err != nil {
    log.Fatalf("read bundle: %v", err)
}

orgID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
inst, secret, err := h.Installer.Install(orgID, b)
if err != nil {
    log.Fatalf("install: %v", err)
}
log.Printf("installed %s@%s id=%s (secret len=%d)", inst.AddonKey, inst.Version, inst.ID, len(secret))
```

`Installer.Install` ([`installer/installer.go`](../installer/installer.go)):

1. Validates the manifest against the running `KernelVersion`.
2. Creates the addon's Postgres schema (`addon_tickets`).
3. Applies any versioned SQL migrations shipped in the bundle.
4. For every `model_definitions[]` entry: `CREATE TABLE IF NOT EXISTS` and
   `ADD COLUMN IF NOT EXISTS` (additive sync).
5. Fires lifecycle `OnInstall` then `OnEnable`.
6. Persists the `metacore_installations` row with a fresh per-install
   HMAC secret (returned to the caller, hashed at rest).

There is no separate `metacore migrate` command — install **is** the
migration trigger. Re-running the install on the same bundle is safe.

For models the host needs to address by short key from CRUD URLs, register
the factory after install:

```go
import (
    "github.com/asteby/metacore-kernel/modelbase"
)

app.RegisterModel("tickets", func() modelbase.ModelDefiner {
    // Return a fresh instance that satisfies modelbase.ModelDefiner.
    // Compiled-in models implement the interface directly; for purely
    // declarative addons, hosts typically synthesize an instance from
    // the manifest (dynamic.BuildStructType + a small ModelDefiner shim).
    return &tickets.Ticket{}
})
```

See [`dynamic-system.md`](dynamic-system.md) for the full installer
walkthrough and how the registry feeds the dynamic CRUD layer.

## 6. Verify the dynamic CRUD endpoints

```bash
# Authenticate (replace with your auth flow).
JWT="$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"secret"}' \
  http://localhost:3000/api/auth/login | jq -r .data.token)"

# Probe metadata.
curl -s -H "Authorization: Bearer $JWT" \
  http://localhost:3000/api/metadata/table/tickets | jq

# Create.
curl -s -X POST -H "Authorization: Bearer $JWT" -H "Content-Type: application/json" \
  -d '{"subject":"Test","status":"open","priority":"normal"}' \
  http://localhost:3000/api/dynamic/tickets | jq

# List.
curl -s -H "Authorization: Bearer $JWT" \
  "http://localhost:3000/api/dynamic/tickets?per_page=10&sortBy=created_at&order=desc" | jq
```

Expected list response:

```json
{
  "success": true,
  "data": [ /* tickets */ ],
  "meta":  { "total": 1, "page": 1, "per_page": 10, "last_page": 1 }
}
```

The full request/response reference is in [`dynamic-api.md`](dynamic-api.md).

If you get `{"success": false, "message": "permission denied: ..."}`, the
user lacks the relevant capability — seed a role grant:

```go
_ = permStore.GrantRole(ctx, permission.RoleAdmin, permission.Cap("tickets", "create"))
_ = permStore.GrantRole(ctx, permission.RoleAdmin, permission.Cap("tickets", "read"))
_ = permStore.GrantRole(ctx, permission.RoleAdmin, permission.Cap("tickets", "update"))
_ = permStore.GrantRole(ctx, permission.RoleAdmin, permission.Cap("tickets", "delete"))
```

See [`permissions.md`](permissions.md) for the complete capability model.

## 7. Pair with a frontend

Frontends running `@asteby/metacore-runtime-react` consume the metadata +
CRUD endpoints with no per-model code:

```tsx
import { DynamicTable } from "@asteby/metacore-runtime-react";

export default function TicketsPage() {
  return <DynamicTable model="tickets" />;
}
```

Hook the runtime up to your host's base URL and JWT — the SDK guide at
`metacore-sdk/docs/CONSUMER_GUIDE.md` covers the React integration end to
end. The contract between this kernel and the SDK is the JSON shape of
`TableMetadata`, `ModalMetadata`, the dynamic CRUD response envelope and
the WebSocket message format — all stable across minor versions.

## Next steps

- [`dynamic-system.md`](dynamic-system.md) — what really happens when an
  addon ships `model_definitions[]`.
- [`dynamic-api.md`](dynamic-api.md) — every endpoint, every parameter.
- [`permissions.md`](permissions.md) — user gates, addon gates, modes.
- [`CONSUMER_GUIDE.md`](CONSUMER_GUIDE.md) — long-form embedding guide.
- [`dev-setup.md`](dev-setup.md) — contributing to the kernel itself.
