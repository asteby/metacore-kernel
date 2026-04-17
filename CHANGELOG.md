# Changelog — metacore-kernel

All notable changes to this module are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [0.1.0] - 2026-04-17

### feat(migrations): configurable Dialect field

- `Runner` now has a `Dialect goose.Dialect` field. Defaults to
  `goose.DialectSQLite3` when zero-value for full backward compatibility.
- Consumers (hub, ops, link) can set `Runner{Dialect: goose.DialectPostgres}`
  without any other change.
- New unit test `TestRunnerDialect_SQLite3Explicit` covers explicit dialect.
- New integration test `TestRunnerDialect_Postgres` (build tag `integration`,
  skipped unless `TEST_POSTGRES_DSN` is set) covers a real Postgres round-trip.

### feat(log): net/http HTTPMiddleware

- Added `log.HTTPMiddleware(logger *slog.Logger) func(http.Handler) http.Handler`
  for chi / net/http consumers (hub). Mirrors FiberMiddleware behaviour:
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
  for domain-specific claim structs (e.g. hub marketplace Plan/Features).
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
- `push.GenerateVAPIDKeys` migrated to `crypto/ecdh` (matches ops/link); the
  public key is now the canonical 65-byte uncompressed P-256 point browsers
  expect from `PushManager.subscribe`.
- `push.Payload` extended with `Image`, `Actions []Action`, `Vibrate`,
  `Silent`, `Renotify` — matching the full Web Notification API surface used
  by ops and link.
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
  predicate.  This is the generic equivalent of link's conversation-aware
  "smart broadcast" (`SendSmartMessage`) — the predicate receives
  `Client.Context` (any), which apps set via `Client.SetContext(v any)`.
- Added `Client.Context any` field + `SetContext` / `GetContext` helpers
  (mutex-protected) for per-connection app state.
- `Hub.SendToUsers([]uuid.UUID, msg)` was already the generic equivalent of
  ops/link's `SendToOrganization` — callers query their own DB for user IDs
  and pass the slice; the hub stays ORM-free.
- `OnNotification` hook covers notification persistence (ops/link handled this
  inline in the hub against their own models; the kernel delegates it).
- Keepalive: `client.go` ping/pong with 60 s pong-wait + (54 s) ping-period
  already matches ops/link behavior.
- **Coverage verdict**: the kernel ws hub covers all routing patterns ops and
  link require (user routing, batch/org broadcast, keepalive, custom message
  types, persistence hook, conditional routing).

### Stable packages (no API changes this cycle)

`modelbase`, `metadata`, `permission`, `dynamic`, `query`, `webhooks`,
`security`, `host`, `installer`, `lifecycle`, `navigation`, `runtime/wasm`.

---

## [v0.2.0-alpha.1] — previous release

_(see git tags for history)_
