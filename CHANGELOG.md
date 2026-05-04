# Changelog — metacore-kernel

All notable changes to this module are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [Unreleased]

### Added

- **`manifest.ActionTrigger` + `ActionDef.Trigger`** (GAP-3). Action
  definitions can now declare an explicit dispatch shape instead of
  relying on the implicit Hooks-map → webhook resolution. `Trigger.Type`
  is one of `wasm` (in-process module export, optionally inside the
  request DB transaction via `RunInTx`), `webhook` (legacy HTTP hop) or
  `noop` (UI-only marker the kernel records for observability).
  `Manifest.Validate` enforces the per-type contract: `wasm` requires
  `Export` and the symbol MUST appear in `Backend.Exports`; `webhook`
  and `noop` reject `Export`/`RunInTx` because the network hop and the
  no-op shape cannot honour them. Additive: the field is a pointer and
  manifests that omit it keep the legacy behaviour, so existing addons
  validate unchanged. Consumers (`bridge/actions.go`, `runtime/wasm`)
  pick the new field up incrementally in follow-up PRs.

### Removed

- **`flow` package — extracted to consumer (link).** The workflow DAG engine
  was domain-specific to link's conversation routing (no other host imported
  it). Cross-repo recon confirmed link was the only consumer; the engine is
  now part of `link/backend/internal/flow` (asteby-hq/link#129). Hosts that
  need a similar primitive should fork from link or implement their own.
  **Breaking change** — consumers upgrading past this version must remove
  any import of `github.com/asteby/metacore-kernel/flow`.

---

## [0.2.0] - 2026-04-24

Major feature release. Consolidates the second wave of ecosystem migration:
anything two or more apps were reimplementing is now owned by the kernel.

### feat(dynamic): Options + Search

- `Service.Options(ctx, user, OptionsQuery) *OptionsResult` and
  `Service.Search(ctx, user, SearchQuery) []Option` replace the
  hand-rolled handlers every app kept in `backend/handlers/options.go`
  and `backend/handlers/search.go`.
- Configurable via `OptionsConfigResolver`, `SearchConfigResolver`,
  `ModelResolver` and `SearchMatchClause` hooks. Default matcher is
  portable (`<col> LIKE ?`); Postgres apps override with
  `unaccent(<col>) ILIKE unaccent(?)`.
- `Handler.MountOptions(r)` exposes `/options/:model` and `/search/:model`
  with the same response envelope legacy handlers returned.
- Option projection DTO (id/value/label/name/description/image/color/icon)
  unified so `DynamicSelect` frontend components consume one shape.
- Model lookup falls back to `gorm.Statement.Parse` when a model does
  not override `TableName()` — no more forcing every gorm model to
  implement `modelbase.ModelDefiner`.

### feat(modelbase): SearchConfig / OptionsConfig types

- `SearchConfig`, `OptionsConfig`, `FieldOptionsConfig` and
  `StaticOption` now live in `modelbase` alongside `TableMetadata`,
  `ModalMetadata`, `FieldDef`, `ActionDef` and friends. Apps alias them
  the same way — `type SearchConfig = modelbase.SearchConfig` — and
  drop their parallel struct definitions.
- Re-exported from `kernel/dynamic` for service callers that prefer the
  behavioural package path.

### feat(httpx): framework-agnostic context extraction

- New `kernel/httpx` package with `ContextLookup` interface (single
  `Locals(key string) any` method) + `ExtractOrgID`, `ExtractUserID`
  and a reflection-based `GetFieldValue`. Apps plug their framework via
  a minimal adapter (`fiberLookup{c}.Locals`) and drop ~60 LOC of
  duplicated extraction helpers.

### feat(push): BroadcastToOrg + OnExpiredEndpoint hook

- `Service.BroadcastToOrg(ctx, tenantID, TenantResolver, Payload)`
  drives concurrent fanout when the resolver returns org-scoped
  subscriptions; apps stop reimplementing WaitGroup loops.
- `Config.OnExpiredEndpoint` hook fires when the provider returns
  404/410, replacing the legacy per-app post-Send `isExpiredEndpoint`
  check that never actually fired (status was returned separately from
  the error). Soft-delete semantics are now the app's choice.
- `IsExpiredStatus(status int) bool` exported helper.

### feat(strings): TitleCase helper

- New `kernel/strings` package with `TitleCase`, replacing a 96-LOC
  `utils/helpers.go` that was byte-for-byte identical across multiple
  host applications.

### feat(migrations): AutoMigrate + toposort + reset

- `AutoMigrate(db, models)` two-pass FK-safe migration,
  `AutoMigrateSorted(db, map)` with topological sort by `foreignKey:`
  gorm tags, `TopoSort(map) []any` exposed for inspection, and
  `ResetDatabase(db)` DESTRUCTIVE drop-all (Postgres CASCADE / SQLite
  per-table). All library-only, CLI-invoked from the app — never at
  boot time.
- Apps' `cmd/seed/main.go` shrinks by ~60% after adoption.

### docs(architecture): Law 0

- Codifies the criterion for kernel vs SDK vs app: "would a fresh
  e-commerce/CRM/SaaS app need this on day one?" Yes → kernel.
  "Nice to have" → SDK. "Only this app" → app.

---

## [0.1.0] - 2026-04-17

### feat(migrations): configurable Dialect field

- `Runner` now has a `Dialect goose.Dialect` field. Defaults to
  `goose.DialectSQLite3` when zero-value for full backward compatibility.
- Consumers can set `Runner{Dialect: goose.DialectPostgres}` without any
  other change.
- New unit test `TestRunnerDialect_SQLite3Explicit` covers explicit dialect.
- New integration test `TestRunnerDialect_Postgres` (build tag `integration`,
  skipped unless `TEST_POSTGRES_DSN` is set) covers a real Postgres round-trip.

### feat(log): net/http HTTPMiddleware

- Added `log.HTTPMiddleware(logger *slog.Logger) func(http.Handler) http.Handler`
  for chi / net/http consumers. Mirrors FiberMiddleware behaviour:
  reads/generates `X-Request-ID`, injects child logger via `WithLogger`, logs
  method/path/status/duration/request_id after each request.
- Package docstring updated to note Fiber and net/http middlewares coexist.
- New unit tests in `log/http_middleware_test.go`.

### feat(metrics): net/http HTTPMiddleware

- Added `metrics.HTTPMiddleware(reg *Registry) func(http.Handler) http.Handler`
  for net/http consumers. Increments `http_requests_total` and observes
  `http_request_duration_seconds` exactly like FiberMiddleware.
- Package docstring updated.
- New unit tests in `metrics/http_middleware_test.go`.

### feat(auth): extensible JWT claims (Option B)

- Added `GenerateTokenWithClaims(claims jwt.Claims, secret []byte, ttl time.Duration) (string, error)`
  and `ValidateTokenWithClaims(token string, secret []byte, claims jwt.Claims) error`
  for domain-specific claim structs (e.g. marketplace Plan/Features).
- Default `Claims` struct and `GenerateToken`/`ValidateToken` are unchanged —
  zero breaking changes.
- Package docstring documents the custom-claims pattern.
- New tests: roundtrip, empty secret, wrong secret, missing token with custom claims.

### feat(migrations): versioned runner replacing AutoMigrate

- New `migrations/` package with `Runner` struct exposing `Up`, `UpTo`, `Down`,
  and `Status` methods backed by `pressly/goose v3` with an embedded `fs.FS`
  source — migration SQL is compiled into the binary.
- `migrations.Migration` struct for version/name/applied introspection.
- Initial SQL migrations for all kernel-owned tables: `users`, `organizations`,
  `webhooks`, `webhook_deliveries`, `push_subscriptions`,
  `metacore_installations` (files under `migrations/sqlfiles/`).
- `AppConfig.RunMigrations bool` in `host`: when `true`, `NewApp` calls
  `Runner.Up` instead of GORM `AutoMigrate`; the old path is retained as a
  deprecated fallback for backward compatibility.
- New dependency: `github.com/pressly/goose/v3 v3.27.0`.

### feat(log): structured slog logger + Fiber middleware

- New `log/` package: `log.New(opts)` returns `*slog.Logger` with JSON (production)
  or text (dev) handler selected via `log.Format`; `log.Default()` convenience
  constructor for zero-config production use.
- `log.WithLogger(ctx, l)` / `log.FromContext(ctx)` propagate the request-scoped
  logger through `context.Context`; falls back to `slog.Default()` if absent.
- `log.WithRequestID`, `log.WithUserID`, `log.WithOrgID` attach standard attrs to
  a child logger.
- `log.FiberMiddleware(logger)` Fiber handler: generates/reads `X-Request-ID`,
  injects child logger into `c.Locals("logger")` and `c.UserContext()`, and logs
  every request with method, path, status, duration, and request_id on completion.
- `log.FromFiberCtx(c, fallback)` retrieves the injected logger from Fiber context.
- `AppConfig.Logger *slog.Logger` added to `host.AppConfig`; auto-initialized to
  `log.Default()` if nil.

### feat(metrics): Prometheus registry + /metrics endpoint

- New `metrics/` package: `metrics.NewRegistry()` returns a `*Registry` with a
  private `prometheus.Registry` and pre-registered metrics:
  `http_requests_total` (counter, labels: method/path/status),
  `http_request_duration_seconds` (histogram, labels: method/path),
  `ws_connections` (gauge), `webhook_deliveries_total` (counter, label: status),
  `push_sends_total` (counter, label: status). Go runtime + process collectors included.
- `metrics.FiberMiddleware(reg)` increments counters and observes latency per request.
- `metrics.Handler(reg)` exposes the registry at `/metrics` in Prometheus text format.
- `AppConfig.EnableMetrics bool` added to `host.AppConfig`; when true, mounts the
  middleware and `GET /metrics` handler via `host.App.Mount()`.
- Added `github.com/prometheus/client_golang v1.23.2` to `go.mod`.

### feat(push): real AES128GCM encryption and proper VAPID JWT

- `push.Service.Send` now performs full RFC 8291 payload encryption (HKDF +
  AES-GCM) and signs the Authorization header with a proper ES256 VAPID JWT
  (RFC 8292).  When no VAPID private key is configured the method falls back to
  plain JSON delivery, preserving backwards compatibility for tests.
- New `push/crypto.go` package-private helper: `encryptPayload` implements the
  `aes128gcm` content-encoding used by all modern push services.
- `push.GenerateVAPIDKeys` migrated to `crypto/ecdh`; the public key is now
  the canonical 65-byte uncompressed P-256 point browsers expect from
  `PushManager.subscribe`.
- `push.Payload` extended with `Image`, `Actions []Action`, `Vibrate`,
  `Silent`, `Renotify` — matching the full Web Notification API surface
  required by typical host applications.
- New `push.Action` type in `models.go`.
- New unit tests: `TestGenerateVAPIDKeys`, `TestVAPIDJWT`, `TestEncryptPayload`
  (all in `push/crypto_test.go`).
- **Decision**: completed `push/` in-place — no separate `webpush/` package
  needed.  The existing package already had VAPID key-gen, subscribe/unsubscribe,
  handler and service tests; only the crypto layer was missing.

### feat(ws): hub confirmed generic; SendConditional added

- `ws.Hub` already used `MessageType string` (not a hardcoded enum), so each
  app can declare its own message-type constants without any kernel change.
  This was confirmed correct and documented in the package-level docstring.
- Added `Hub.SendConditional(userID, predicate, primary, fallback)`: delivers
  different messages to a user's connections based on a per-connection
  predicate.  This is the generic equivalent of a conversation-aware
  "smart broadcast" — the predicate receives `Client.Context` (any), which
  apps set via `Client.SetContext(v any)`.
- Added `Client.Context any` field + `SetContext` / `GetContext` helpers
  (mutex-protected) for per-connection app state.
- `Hub.SendToUsers([]uuid.UUID, msg)` is the generic equivalent of an
  org-scoped broadcast — callers query their own DB for user IDs and pass
  the slice; the hub stays ORM-free.
- `OnNotification` hook covers notification persistence (the kernel delegates
  it; hosts handle persistence inline against their own models).
- Keepalive: `client.go` ping/pong with 60 s pong-wait + (54 s) ping-period
  matches typical browser-friendly defaults.
- **Coverage verdict**: the kernel ws hub covers all routing patterns host
  applications typically require (user routing, batch/org broadcast,
  keepalive, custom message types, persistence hook, conditional routing).

### Stable packages (no API changes this cycle)

`modelbase`, `metadata`, `permission`, `dynamic`, `query`, `webhooks`,
`security`, `host`, `installer`, `lifecycle`, `navigation`, `runtime/wasm`.

---

## [v0.2.0-alpha.1] — previous release

_(see git tags for history)_
