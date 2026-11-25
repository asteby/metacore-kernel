# apitoken: namespace, pepper and hash schemes

Package `apitoken` issues scoped API tokens (`pk_`, `sk_`, `pt_`). Two opt-in
options let a deployment mark its tokens and protect the stored hash. Both are
off by default: a `Service` built with no options behaves byte for byte like
the previous release.

## Options

| Option | Effect |
|---|---|
| `WithNamespace("sup")` | Plaintext prefix becomes `<ns>_<kind>_` (`sup_sk_...`). Namespace: 2-8 lowercase letters/digits, starts with a letter, no underscore; invalid values panic at construction. A namespaced service neither issues nor authenticates un-namespaced tokens (and the reverse). The display `Prefix` keeps `PrefixLen` characters plus the namespace and separator, capped at the 16-char column. |
| `WithPepper(func() []byte)` | New tokens are stored as `hmac-sha256$<hex>`, HMAC-SHA256 of the plaintext keyed by the pepper. The function is called on each hash, so the host can read a secret store. If it returns no key, `Issue`/`Rotate` fail with `ErrPepperUnavailable` (never a silent downgrade to an unpeppered hash). |
| `WithSchemedHash()` | Without a pepper, store `sha256$<hex>` instead of the legacy bare hex. Off by default so a binary rollback keeps authenticating. Ignored when a pepper is set. |

## `token_hash` column

| Stored value | Written by | Verified by |
|---|---|---|
| `<64 hex>` (no scheme) | every release before schemes; default today | always |
| `sha256$<64 hex>` | `WithSchemedHash()` | always |
| `hmac-sha256$<64 hex>` | `WithPepper(...)` | only with the same pepper |

Verification computes every form of the presented plaintext (the peppered one
only when a pepper is available), loads the row by any of them in one query
(`HashesStore`, implemented by `MemoryStore` and `GormStore`; other stores are
queried once per form), then re-compares the stored value against all forms
with `crypto/subtle.ConstantTimeCompare`, without short circuit. Malformed
tokens are rejected before reaching the store. Errors are fixed values
(`ErrInvalidToken`, ...): neither the plaintext, the digest nor the pepper is
ever put in an error or a log line.

The column is now `size:96` (a scheme-qualified HMAC is 76 characters).
`Migrate` widens it; on Postgres `AutoMigrate` runs `ALTER COLUMN ... TYPE
varchar(96)`, which only widens. Run `Migrate` before enabling a pepper.

## Rotation

`Rotate(ctx, id, grace)` is unchanged: the replacement is issued with the
service's current namespace and hash form, the old token keeps its own hash and
authenticates until `now+grace`. This is also the adoption path: rotating a
legacy token with a peppered service yields a peppered token while the old one
still works during the grace window.

Changing the pepper invalidates every token stored under the old one. To retire
a pepper, roll the affected tokens with `Rotate` first. The package does not
accept several peppers at once.

## Adoption guide (no migration is done by the kernel)

### OPS `apigateway`

Nothing to change: OPS builds `apitoken.NewService(apitoken.NewGormStore(db))`
(`appserver/api_gateway.go`, `appserver/server.go`), which keeps writing bare
hex and `sk_`/`pk_`/`pt_` prefixes. Before bumping the kernel in OPS:

1. Add a regression test in `backend/apigateway`: seed a token row with the
   bare hex hash produced by `apitoken.HashToken(plain)` (and one through the
   previous release's `Issue`), then `Authenticate` through the new kernel and
   through the gateway path, expecting the same principal. Cover pk/sk/pt.
2. Run `Migrate`; the only schema change is `token_hash` varchar(64) to (96).
3. `backend/cmd/devctl/bootstrap.go` compares `t.TokenHash == apitoken.HashToken(plain)`;
   that still holds for default-configured services but would not for a
   peppered or schemed one. If OPS later adopts a pepper, use `Authenticate`
   there instead.
4. To adopt a pepper later: `WithPepper` plus a secret from the platform secret
   store, then rotate existing tokens with a grace window.

### Hub (support channels, WP-18)

Build a dedicated service per credential family:
`apitoken.NewService(store, apitoken.WithNamespace("sup"), apitoken.WithPepper(pepperFn))`
where `pepperFn` reads the deployment pepper (required in production; fail
startup if empty). Keys are `sup_sk_...`. Use `NewPrincipalResolver(svc, ...)`
as usual; a bearer that does not start with `sup_` falls through to the
configured `Fallback`. Do not share a table between the namespaced service and
another one with a different pepper.

## Tests

`apitoken/namespace_pepper_test.go` (table tests over MemoryStore and GormStore
for: legacy hash compatibility, scheme column, pepper present / absent / empty /
rotated, namespace shape and isolation, rotation with grace, malformed tokens,
constant-time comparison, no secret in errors) and
`TestMiddleware_NamespaceAndPepper` in `apitoken/fiber_test.go`.
