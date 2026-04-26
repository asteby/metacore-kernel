# Development setup

This guide is for **contributors** to `metacore-kernel`. If you are embedding
the kernel into an application, read [`CONSUMER_GUIDE.md`](./CONSUMER_GUIDE.md)
instead.

---

## Table of contents

1. [Prerequisites](#1-prerequisites)
2. [Clone and bootstrap](#2-clone-and-bootstrap)
3. [Repository layout](#3-repository-layout)
4. [Tests, vet, race detector](#4-tests-vet-race-detector)
5. [Working with the SDK locally](#5-working-with-the-sdk-locally)
6. [Branching and contribution flow](#6-branching-and-contribution-flow)
7. [Style and architectural guardrails](#7-style-and-architectural-guardrails)
8. [Debugging the WASM runtime](#8-debugging-the-wasm-runtime)
9. [Releasing](#9-releasing)

---

> Embedding the kernel in a host? You want
> [`CONSUMER_GUIDE.md`](CONSUMER_GUIDE.md) and
> [`embedding-quickstart.md`](embedding-quickstart.md). Working on the
> dynamic CRUD framework? Read [`dynamic-system.md`](dynamic-system.md)
> first — fixtures, handler tests, and migration helpers in
> `dynamic/*_test.go`, `metadata/*_test.go`, and `installer/*_test.go`
> assume the contracts documented there.

## 1. Prerequisites

| Tool       | Version                          |
| ---------- | -------------------------------- |
| Go         | 1.25+ (matches `go.mod` and CI)  |
| Git        | 2.30+                            |
| GitHub CLI | recommended for PRs and releases |
| Make       | optional                         |

The kernel depends on `wazero` (pure Go WASM runtime — no cgo), `Fiber v2`,
`GORM`, `gofiber/websocket`, `prometheus/client_golang`, and `goose` for
versioned migrations. There are no native build tools required.

## 2. Clone and bootstrap

```bash
go env -w GOPRIVATE="github.com/asteby/*"
git config --global url."git@github.com:".insteadOf "https://github.com/"

mkdir -p ~/projects && cd ~/projects
git clone git@github.com:asteby/metacore-kernel.git
git clone git@github.com:asteby/metacore-sdk.git
cd metacore-kernel
go mod download
```

The two repos must live as siblings — the kernel's `go.mod` carries
`replace github.com/asteby/metacore-sdk => ../metacore-sdk` so SDK changes
are picked up without publishing a tag.

Verify the toolchain:

```bash
go version          # expect 1.25.x
go vet ./...
go test ./...
```

## 3. Repository layout

```
metacore-kernel/
├── auth/              JWT, login/refresh handlers, Fiber middleware
├── bridge/            Adapters: kernel actions/tools/webhooks ↔ host integrations
├── bundle/            Addon bundle I/O (`bundle.tgz` reader/writer)
├── docs/              Developer-facing documentation (this directory)
├── dynamic/           Generic CRUD over registered models
├── eventlog/          Org-scoped persisted event log with cursor pagination
├── events/            In-process pub/sub bus for addons
├── flow/              Workflow primitives reused by addons
├── host/              `App` and `Host` facades
├── httpx/             HTTP helpers shared across handlers
├── installer/         Install/enable/disable/uninstall flow
├── lifecycle/         Addon contract, registry, interceptors
├── log/               Builder-style logger (legacy; use obs/ for new code)
├── manifest/          Declarative addon manifest schema
├── metadata/          TableMetadata/ModalMetadata registry, cache, handler
├── metrics/           Prometheus integration
├── migrations/        Goose-based versioned migration runner
├── modelbase/         Stable interfaces and base structs
├── navigation/        Sidebar merger
├── notifications/     Delivery queue + workers + ChannelHandler
├── obs/               Structured slog logger with request-id propagation
├── permission/        Role + capability checks
├── push/              Web Push (VAPID)
├── query/             Filter/sort/paginate query builder
├── runtime/wasm/      wazero-based WASM runtime
├── security/          Enforcer, Capabilities, HMAC, secretbox, nonce
├── strings/           Shared string helpers
├── tool/              Addon tool runtime + dispatcher + registry
├── webhooks/          Outbound HMAC-signed webhooks with retry
├── ws/                WebSocket hub
├── ARCHITECTURE.md    The four laws of the kernel — read before adding a package
├── CHANGELOG.md       Release history
└── README.md          Top-level overview
```

Each package owns its tests (`*_test.go`), a `doc.go` where useful, and a
single coherent responsibility (see `ARCHITECTURE.md`, *Law 0*).

## 4. Tests, vet, race detector

CI runs the same commands you should run locally before opening a PR
(`.github/workflows/ci.yml`):

```bash
go vet ./...
go test -race -coverprofile=coverage.out ./...
```

Useful subset patterns:

```bash
# A single package, verbose
go test -race -v ./runtime/wasm/...

# Watch a single test
go test -race -run TestEnforcer_Shadow ./security/...

# Coverage HTML
go tool cover -html=coverage.out
```

The race detector is **mandatory** — the kernel hosts long-lived goroutines
(WS hub, webhook dispatcher, notification workers) and most regressions
surface only under `-race`.

## 5. Working with the SDK locally

During day-to-day work the kernel resolves the SDK from `../metacore-sdk` via
the replace directive in `go.mod`. To preview the build that consumers will
actually pull:

```bash
go mod edit -dropreplace github.com/asteby/metacore-sdk
go mod tidy
go test ./...
```

Re-add the replace before resuming local work:

```bash
go mod edit -replace github.com/asteby/metacore-sdk=../metacore-sdk
go mod tidy
```

Never commit a `go.mod` without the replace directive on a feature branch —
the release script drops it as part of tagging.

## 6. Branching and contribution flow

- Branch from `main`. Use a descriptive prefix:
  `feat/`, `fix/`, `refactor/`, `docs/`, `chore/`.
- Conventional Commits are enforced for changelog generation. The `feat:` /
  `fix:` / `BREAKING CHANGE:` markers drive the SemVer decision at release
  time (see [`RELEASE.md`](./RELEASE.md)).
- One coherent change per PR. Public-API changes need a corresponding
  `// Deprecated:` comment if they replace existing symbols.
- Open the PR, let CI go green, request review, squash-merge.

## 7. Style and architectural guardrails

The full statement is in [`ARCHITECTURE.md`](../ARCHITECTURE.md). The four
points to internalize before contributing:

1. **Stability by interfaces, not structs.** Every public contract lives
   behind an interface (`AuthUser`, `AuthOrg`, `ModelDefiner`, …). Apps
   extend by composition; the kernel evolves without breaking them.
2. **Opinionated defaults, pluggable escape hatches.** Constructors take a
   `Config`; behavior overrides are `With*` methods, never forks.
3. **Services are mandatory, handlers are optional.** A `service.go` must
   never import `github.com/gofiber/fiber/v2`. Handlers are thin Fiber
   wrappers around services.
4. **What belongs in the kernel.** Substrate that every web app needs on
   day one. Optional reusable infra goes in the SDK; product-specific code
   goes in the app. When in doubt, default to keeping it out.

Additional dependency rules:

- `modelbase/` imports nothing beyond `gorm.io/gorm`, `github.com/google/uuid`,
  `golang.org/x/crypto/bcrypt`. No Fiber, no HTTP, no SDK.
- `obs/` imports only the standard library. It is the most upstream package.
- No kernel package may import `github.com/asteby/metacore-sdk/pkg/*` unless
  the dependency is on a stable public type (manifest, bundle schema).

## 8. Debugging the WASM runtime

The WASM runtime lives in `runtime/wasm/`. A handful of patterns that pay
off when diagnosing addon failures:

- **Reproduce in `wasm_test.go`.** The package ships a fixture that compiles
  a tiny Go-to-WASM module and runs it through the full ABI; copy that test
  and add the failing scenario.
- **Inspect host imports.** `capabilities.go` registers every host import.
  If the addon's `imported function not found` errors at instantiation time,
  the symbol name is missing from `registerHostModule`.
- **Check the enforcer mode.** Locally the default is `ModeShadow`; turn on
  `ModeEnforce` (`METACORE_ENFORCE=1`) when chasing capability bugs so they
  surface as errors instead of warnings.
- **Memory and timeouts.** Defaults are 64 MiB / 10 s per invocation, with a
  global 256 MiB ceiling on the runtime config. Override per-addon via
  `manifest.BackendSpec`.

## 9. Working on the dynamic CRUD framework

Most kernel changes that affect consumer apps land in `dynamic/`,
`metadata/`, `permission/`, `installer/`, or `manifest/`. A few patterns
that pay off when iterating there:

- **End-to-end fixture per package.** `dynamic/service_test.go` builds an
  in-memory SQLite DB, registers a fake model, and exercises Create / Get /
  List / Update / Delete. Copy that fixture to add a regression test for
  a new code path; do not stand up Postgres unless you are testing RLS or
  a Postgres-only feature.
- **Handler tests use `app.Test()`.** `metadata/handler_test.go` shows the
  pattern: build a Fiber app, mount the handler, send `httptest`-style
  requests, assert the JSON envelope. Keep handler tests as thin as
  possible — service-level tests cover correctness, handler tests cover
  status codes and the wire envelope.
- **Manifest fixtures live in tests.** `manifest/validate_test.go` and
  `installer/dualwrite_test.go` declare manifests inline. There is no
  separate fixture directory; if you need a complex one, add it next to
  the test that uses it.
- **Schema-affecting changes touch three places.** Adding a column type
  to `dynamic/model.go:columnGoType` also requires `dynamic/schema.go:pgColumnType`
  and a corresponding entry in [`dynamic-system.md`](dynamic-system.md)
  *Allowed column types*. Renaming or removing a column type is a MAJOR
  bump because addon manifests in the wild depend on it.
- **Public response shapes are wire contracts.** The JSON tags on
  `modelbase.TableMetadata`, `modelbase.ModalMetadata`, the
  `dynamic.Handler` envelope (`{success, data, meta}`) and `query.PageMeta`
  are stable across minors. Adding a field is fine; removing or renaming
  one is a MAJOR.

## 10. Releasing

The release process — version selection, tag publication, GoReleaser,
consumer dispatch, retract — is documented end-to-end in
[`RELEASE.md`](./RELEASE.md). In short: `git push origin vX.Y.Z` runs the
release workflow, which runs the test suite, indexes the proxy, publishes a
GitHub Release and notifies every consumer repository.
