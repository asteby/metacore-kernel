# metacore-kernel — Architecture Principles

This document codifies the three laws of the kernel. Every module added here must
respect them. Apps (ops, link, pilot, doctores.lat, p2p, etc.) consume this
kernel as a semver Go module — breakage in the kernel breaks every app at once.

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
- `auth/`, `permission/`, `dynamic/`, `metadata/`, `webhooks/` may import
  `modelbase/` and third-party libs. They **must not** import each other except
  through interfaces.
- No kernel package may import `github.com/asteby/metacore-sdk/pkg/*` unless the
  dependency is on a stable public type (manifest, bundle schema).

## Directory contract

```
metacore-kernel/
├── modelbase/       # types + interfaces — the foundation
├── auth/            # authn + JWT + middleware + handler
├── permission/      # authz — role + capability checks
├── metadata/        # TableMetadata/ModalMetadata registry + handler
├── dynamic/         # generic CRUD by metadata + handler
├── query/           # filter/sort/paginate query builder
├── webhooks/        # outbound webhook dispatch + retry
├── push/            # web push (VAPID)
├── host/            # boot orchestration (glue)
├── lifecycle/       # addon lifecycle registry
├── installer/       # addon install + migration runner
├── navigation/      # sidebar merger
├── events/          # in-process pub/sub
├── security/        # HMAC signing + capability enforcement
└── runtime/wasm/    # WASM addon runtime (wazero)
```

Each module owns its tests, its doc.go with a usage example, and its errors.
