# Instance licensing (`licensing/`)

The `licensing` package is metacore's **instance-licensing primitive**: the
shared enforcement half of the platform's trust chain, living at the base of
the kernel so every product — ops, the verticals, self-contained appliances —
is licensed for free and identically. It is a faithful, embedder-agnostic port
of the ops consumer (`services/instancelicense`); the JSON wire format is the
hub's real one, so a token minted by the hub verifies here byte-for-byte
unchanged.

The license authorizes the **instance to exist**. It is deliberately generic:
it never limits *what* you may install beyond the entitlement set the hub chose
to grant, and it is not tied to any one vertical. The lease + check-in loop is
telemetry and the lever that makes remote revocation effective.

## Division of responsibility

| Layer | Repo | Responsibility |
|-------|------|----------------|
| **Emission / trust anchor** | hub | Holds the Ed25519 license private key. Mints, activates and renews tokens; publishes the public key at `GET /v1/license/pubkey`; runs the fleet check-in registry. |
| **Enforcement** | kernel (`licensing/`) | Verifies tokens **offline** against the hub pubkey, caches the entitlement snapshot, re-checks + renews on a schedule, and exposes pure gate decisions. This package. |
| **UX** | SDK (`<LicenseGate>`) | Renders the `State` snapshot: blocking activation modal when locked out, degraded banner in stale/grace, children untouched when valid or unenforced. |
| **Wiring** | hosts (ops, appliances) | Implement `LicenseStore`, call `Boot`/`Start`, and hang `WritesBlocked` / `InstallBlocked` / `Entitles` off their request pipeline. |

Kernel and SDK are **independent releases**: they share only the shape of the
`State` JSON. A host can upgrade one without the other.

## Token format (offline-verifiable)

```
token      = base64url(claimsJSON) + "." + base64url(ed25519Sig)
claimsJSON = compact json.Marshal of licensing.Claims
sig        = ed25519.Sign(hubLicensePriv, claimsJSON)   // over RAW claims bytes
```

Verification signs/verifies the **raw** decoded claims bytes (never a
re-marshal) to avoid canonicalization drift — identical rule on both sides.

```jsonc
{
  "v": 1,                       // token version; rejected if != 1
  "typ": "instance",            // discriminator; trial/annual tokens are rejected
  "lid": "<uuid>",              // license id
  "cid": "<uuid>",              // customer id
  "iid": "<uuid>",              // bound instance id (set at activation)
  "ipk": "<hex>",               // bound instance Ed25519 pubkey
  "plan": "pitsline",           // commercial plan; "unlimited" = first-party perpetual
  "presets": ["pitsline"],      // entitled preset keys
  "addons": ["workshop", ...],  // entitled addon keys; ["*"] = wildcard
  "iat": "<RFC3339 UTC>",       // re-stamped on every renew (= last check-in)
  "exp": "<RFC3339 UTC>",
  "grace": 7,                   // days of degraded operation past exp
  "moh": 72                     // LEASE: max hours without a check-in; 0 = none
}
```

## Posture state machine

`licensing.Service` re-derives one immutable `State` snapshot on each check and
swaps it atomically. Postures and what enforcement blocks:

| Status | Meaning | Reads | Writes | Installs |
|--------|---------|:-----:|:------:|:--------:|
| `valid` | Verified and inside the window. | ✅ | ✅ | ✅ (entitled keys) |
| `stale` | Lease expired (`now > iat + moh`); still inside the window. Operable but degraded — renew required. | ✅ | ✅ | ✅ |
| `grace` | Past `exp`, within `exp + grace`. Degraded; renew required. | ✅ | ✅ | ✅ |
| `expired` | Past `exp + grace`. | ✅ | ⛔ | ⛔ |
| `missing` | No token installed. | ✅ | ⛔ | ⛔ |
| `invalid` | Signature/version/type check failed. | ✅ | ⛔ | ⛔ |

The `unlimited` plan (first-party, wildcard) never expires nor goes stale.
Enforcement is a flag: with `Enforce=false` the `State` is still computed and
surfaced, but nothing is blocked (`Operable()` is always true). Reads and a
small recovery surface (auth, the license admin API, health) must stay open in
every posture so an operator can always paste a fresh token.

Cutting the hub off does **not** evade revocation: without renews a leased
license goes `stale` within `moh` hours and lapses at `exp + grace`.

## Renewal = mandatory check-in

When linked (`InstanceID` + `InstancePriv`) and online, the service renews via
`POST /v1/licenses/renew`, signing the request with the instance key
(`X-Signature: ed25519:...`, message = `sha256(body || "\n" || unix_ts)`) — the
durable instance-signature scheme, not an expiring JWT. A **leased** license
renews on *every* cycle (default 6h); a non-leased one only within 30 days of
expiry. The hub re-issues the same entitlement with a fresh window (`iat`
re-stamped) and records the check-in. Offline appliances leave the instance key
unset and run on their local window (issue them with `moh: 0`).

## Embedding

```go
store := licensing.NewFileStore("/var/lib/app/license.json") // or a GORM adapter
cfg := licensing.NewConfigFromEnv() // reads LICENSING_ENFORCE, OPS_LICENSE_*, HUB_BASE_URL, ...
cfg.Store = store
svc := licensing.New(cfg)

svc.Boot(ctx)   // adopt persisted/bootstrap token, publish first snapshot
svc.Start(ctx)  // background re-check + renew loop (cancel ctx to stop)

// In middleware:
if svc.Current().WritesBlocked() { /* 402 license_expired on non-GET */ }
// In the install/claim handler:
st := svc.Current()
if st.InstallBlocked() || !st.Entitles(addonKey) { /* 402 not_entitled */ }
// Admin surface:
state, err := svc.Activate(ctx, pastedToken)
```

`LicenseStore` is the only piece an embedder must supply:

```go
type LicenseStore interface {
    Load(ctx context.Context) (*Record, error) // nil,nil when none installed
    Save(ctx context.Context, rec *Record) error
}
```

`MemoryStore` (tests/ephemeral) and `FileStore` (appliances) ship in the
package; ops adapts its GORM singleton row.
