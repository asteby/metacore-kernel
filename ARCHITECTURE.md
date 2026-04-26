# metacore-kernel — Architecture Principles

This document codifies the four laws of the kernel. Every module added here must
respect them. Host applications consume this kernel as a semver Go module —
breakage in the kernel breaks every host at once.

## Law 0 — What belongs in the kernel

The kernel is the **minimum viable substrate** for a multi-tenant web application.
Three buckets, only one of them lives here:

| Bucket | Definition | Where it lives |
|---|---|---|
| **Substrate** | If you remove it, no app starts. Every web app needs it. | **kernel** |
| **Optional infra** | Reusable across apps but not required to boot. Pluggable. | **SDK Go / SDK TS** |
| **Domain** | Specific to a product (chat, ticketing, agents, sales). | **app** |

The mental test before adding a package to the kernel:

> *Would a fresh e-commerce / CRM / SaaS app, with zero relationship to the
> existing apps, need this on day one to function?*

- Yes → kernel.
- "It would benefit from it" → SDK.
- "Only this app needs it" → app.

### What's substrate (kernel)

- `modelbase` — base types, every model embeds them.
- `auth` — login, JWT, middleware. No app boots without identity.
- `permission` — authz. Required to gate every endpoint.
- `metadata`, `dynamic`, `query` — metadata-driven CRUD. The DNA of metacore.
- `host`, `lifecycle`, `navigation` — boot orchestration.
- `obs` — structured logging. No service ships without logs.

### What's infra in the kernel today (acceptable but evaluate before extending)

These are technical communication primitives every web app eventually wants;
they live in the kernel for consistency with how apps integrate:

- `push` (Web Push VAPID), `ws` (WebSocket Hub), `webhooks` (outbound HMAC),
  `notifications` (queue + workers + dedup + retry + ChannelHandler).

The **engine** is in the kernel; **domain handlers/templates/rules stay in the
app**. The notifications service ships a queue; the app registers what an "email"
or "whatsapp" handler does. The push service ships VAPID; the app decides what
events trigger a push.

### What's an addon platform (kernel only if it's the product thesis)

`tool`, `bundle`, `installer`, `manifest`, `security`, `runtime/wasm`,
`events` (addon bus). These exist because metacore positions itself as an
**addon platform** — apps assemble functionality from federated WASM addons.
If a future direction makes addons opt-in, these would move to a separate
module. Today they're kernel because they're the product thesis.

### Things that explicitly DO NOT belong in the kernel

- Templates / template renderers with domain conventions (e.g. Mustache
  aliases, autovars, fallback chains tailored to a specific app)
- Rule engines tied to specific entity types (`AgentTool`, `Conversation`,
  `Ticket`, `Contact`)
- Channel implementations with provider-specific protocols (WhatsApp/Baileys,
  Twilio, Mailgun) — the **interface** `ChannelHandler` is in the kernel; each
  app wires its providers
- Web framework middleware (Fiber-specific request_id, panic recovery flavored
  for an app's error contract) — kernel exposes pure-`context.Context` services;
  apps glue them to their HTTP framework

### Why this matters

Every package added to the kernel is a forced decision for every future app.
Breaking changes in the kernel break every app at once. Bloat translates to
binary size, slower CI for the kernel, more code to audit, and harder
onboarding ("what is all this?"). When in doubt, default to keeping it out —
promotion later is cheap; demotion later is painful.

## Law 1 — Stability by interfaces, not structs

Every public contract the kernel exposes lives behind an **interface** in
`modelbase/` or the owning package. Structs are extension points (embed them);
interfaces are the stable API.

```go
// CONTRACT (stable cross-version):
type AuthUser interface {
    GetID() uuid.UUID
    GetOrganizationID() uuid.UUID
    GetEmail() string
    GetRole() string
}

// DEFAULT IMPLEMENTATION (extensible via embedding):
type BaseUser struct {
    BaseUUIDModel
    Email        string
    PasswordHash string
    Role         string
    // ...
}

// Apps extend by composition — adding fields never breaks the kernel:
type MyAppUser struct {
    modelbase.BaseUser
    BranchID     *uuid.UUID   // app-specific, invisible to kernel
    CheckoutMode string
}
```

**Rule:** kernel services (auth, dynamic, permission, webhooks) accept the
**interface**, never the struct. Adding a field to `BaseUser` is a non-breaking
change. Changing the interface signature is a **MAJOR** version bump and
requires a deprecation cycle.

## Law 2 — Opinionated defaults, pluggable escape hatches

Every service ships defaults that make 90% of apps work with zero config. The
other 10% override via `With*` options — never by forking.

```go
svc := auth.New(db, auth.Config{JWTSecret: secret})          // works for most
svc = svc.WithUserModel(func() AuthUser { return &MyUser{} })// app-specific
svc = svc.WithPostLoginHook(setBranchCookie)                 // opt-in behavior
```

**Rule:** if an app has to fork a kernel file to change behavior, the kernel has
a bug. Parameterize via constructor options, hooks, or policy interfaces.

## Law 3 — Services are mandatory, handlers are optional

Every module exposes two layers:

- **Service** (framework-agnostic) — pure Go, accepts `context.Context`, returns
  errors. Callable from Fiber, gin, echo, gRPC, AWS Lambda, a CLI, or tests.
- **Handler** (Fiber, optional) — thin wrapper that reads request, calls
  service, writes response. Can be swapped without touching service.

```go
// This works everywhere:
result, err := authService.Login(ctx, auth.LoginInput{...})

// This only works with Fiber:
authHandler.Mount(app.Group("/api/auth"))
```

**Rule:** never import `github.com/gofiber/fiber/v2` from a `service.go`. If a
future consumer uses Echo or gRPC, they consume the service directly and write
their own handler. The kernel must not presume the transport.

## Semver discipline

| Change                                        | Bump  |
|-----------------------------------------------|-------|
| Adding a field to a `Base*` struct            | minor |
| Adding an optional `With*` option             | minor |
| Adding a method to an interface               | **major** |
| Changing a method signature on an interface   | **major** |
| Removing or renaming an exported symbol       | **major** |
| Bug fix, internal refactor, new test          | patch |

## Dependency policy

- `modelbase/` imports nothing except `gorm.io/gorm`, `github.com/google/uuid`,
  `golang.org/x/crypto/bcrypt`. No Fiber, no HTTP, no SDK.
- `obs/` imports only the standard library (`log/slog`, `context`). It is the
  most upstream package — every other kernel module may import it.
- `auth/`, `permission/`, `dynamic/`, `metadata/`, `webhooks/`, `push/`, `ws/`,
  `notifications/`, `eventlog/` may import `modelbase/` + `obs/` + third-party
  libs. They **must not** import each other except through interfaces.
- `notifications/` exposes `ChannelHandler` interface; concrete handlers
  (email/SMS/push/whatsapp) live in apps, never in the kernel.
- `eventlog/` accepts `Tags map[string]string` for opaque domain IDs; never
  holds FKs to app-specific tables.
- No kernel package may import `github.com/asteby/metacore-sdk/pkg/*` unless the
  dependency is on a stable public type (manifest, bundle schema).
- No kernel package may import `github.com/gofiber/fiber/v2` from a `service.go`
  (see Law 3).

## Directory contract

```
metacore-kernel/
├── modelbase/       # types + interfaces — the foundation
├── auth/            # authn + JWT + middleware + handler
├── permission/      # authz — role + capability checks
├── metadata/        # TableMetadata/ModalMetadata registry + handler
├── dynamic/         # generic CRUD by metadata + handler
├── query/           # filter/sort/paginate query builder
├── obs/             # structured logger with context propagation (slog)
├── webhooks/        # outbound webhook dispatch + retry
├── push/            # web push (VAPID)
├── ws/              # WebSocket Hub + Client + broadcast
├── notifications/   # delivery queue: workers + dedup + retry + ChannelHandler
├── eventlog/        # org-scoped persistent pub/sub with cursor pagination
├── host/            # boot orchestration (glue)
├── lifecycle/       # addon lifecycle registry
├── installer/       # addon install + migration runner
├── navigation/      # sidebar merger
├── events/          # in-process addon pub/sub bus (NOT eventlog — different concern)
├── tool/            # addon tool runtime + dispatcher + registry
├── manifest/        # addon manifest schema
├── bundle/          # addon bundle I/O contracts
├── security/        # HMAC signing + capability enforcement
├── log/             # builder-style logger (legacy; new code uses obs)
├── metrics/         # Prometheus metrics helpers
├── migrations/      # GORM migration helpers
└── runtime/wasm/    # WASM addon runtime (wazero)
```

Each module owns its tests, its doc.go with a usage example, and its errors.

### Naming collision notes

- `events/` is the **in-process addon bus** (publish/subscribe with capability
  checks via `security.Enforcer`, no persistence, wildcard patterns). It serves
  the addon runtime.
- `eventlog/` is the **org-scoped persisted event log** with cursor-based
  pagination. It serves apps that need event sourcing, audit trails, or
  org-wide notifications. Different concern, hence the different name.

If a future cleanup unifies nomenclature, the rename direction would be
`events/` → `eventbus/` and `eventlog/` → `events/`. That's a major-version
change with migration of every addon callsite — defer until the cost justifies it.
