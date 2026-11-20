# security/signedenvelope: one place to verify Ed25519

Status: phase 1 of 2 (kernel only). Ops and hub are NOT migrated by this
change. Finding H1 of the backend audit: `ed25519.Verify` was re-implemented
in ~30 places, almost none with domain separation.

## 1. Inventory (verified against origin/main, `git grep ed25519.Verify`, non-test code)

Heads: kernel `87b1644`, ops `6291b051`, hub `8160f29`. Counts of real calls:
kernel 5, ops 14, hub 13 (the audit counted comments too).

### Ops

| # | Site | What it verifies | Wire format today |
|---|------|------------------|-------------------|
| 1 | `backend/cmd/appliance/selfupdate_apply.go:242` | appliance self-update (release binary) | sig over the 32 raw bytes of sha256 |
| 2 | `backend/cmd/asteby-bundle/main.go:380` | addon bundle (CLI verify) | sig over the hex STRING of the digest |
| 3 | `backend/cmd/asteby-update/main.go:499` | autoupdate of the asteby binary | sig over the hex string of sha256 (same shape as 2) |
| 4 | `backend/edge/update/update.go:139` | edge release manifest, per platform asset | own domain text + version/build/platform/file/sha/size |
| 5 | `backend/edgesync/devauth.go:69` | edge device request auth (PoP, per-device key) | method\npath\nts\nhex(sha256(body)) |
| 6 | `backend/handlers/hub_orders.go:179` | bundle counter-signature of a hub order | sig over raw sha256(bundle) |
| 7 | `backend/handlers/kernel_marketplace_edge.go:59` | bundle counter-signature (edge install) | sig over raw sha256(bundle) |
| 8 | `backend/services/activationbootstrap/bootstrap.go:194` | activation bootstrap document | `"asteby/act-bootstrap/v1\n"` + doc |
| 9 | `backend/services/addonbundle/signature.go:217` | addon bundle dir digest | sig over hex string of the dir digest |
| 10 | `backend/services/applicense/applicense.go:147` | application licence token | legacy token `b64(claims).b64(sig)` |
| 11 | `backend/services/brandpack/brandpack.go:125` | brand pack | sig over sha256(`Domain` + pack) |
| 12 | `backend/services/hubdir/command.go:93` | hub control command | sig over sha256(body + "\n" + ts) |
| 13 | `backend/services/hubdir/enter.go:102` | hub to instance "enter" token | legacy token |
| 14 | `backend/services/hubordersclient/verify.go:177` | hub order | `"asteby-order-v1\x00"` + envelope bytes |

### Hub

| # | Site | What it verifies | Wire format today |
|---|------|------------------|-------------------|
| 1 | `backend/internal/api/activation_links.go:1192` | activation proof of possession (instance key) | PoP message (link id, pub, challenge) |
| 2 | `backend/internal/api/activation_links.go:1467` | activation bootstrap document | `"asteby/act-bootstrap/v1\n"` + doc |
| 3 | `backend/internal/api/core_auth.go:183` | core API request auth (instance key) | canonical request string |
| 4 | `backend/internal/api/install_handshake.go:817` | install handshake (instance key) | sha256(body + "\n" + ts) |
| 5 | `backend/internal/api/orders_sign.go:116` | order self-check (tests, golden vectors) | `"asteby-order-v1\x00"` + envelope |
| 6 | `backend/internal/license/ed25519license.go:162` | annual on-prem licence | legacy token |
| 7 | `backend/internal/license/envelope.go:127,155` | a hub-local COPY of the sigv2 envelope (legacy + `asteby1.`) | legacy token / `asteby1.` |
| 8 | `backend/internal/license/instancelicense.go:468` | instance token signature (ignoring expiry) | legacy token |
| 9 | `backend/internal/license/triallicense.go:194` | trial licence | legacy token |
| 10 | `backend/internal/marketplace/revocations.go:117` | revocation list | canonical payload, std-base64 sig |
| 11 | `backend/internal/signer/ed25519.go:32` | generic `signer.Verify(pubHex,data,sig)` | any |
| 12 | `backend/internal/signer/signer.go:85` | developer upload signature (developer key) | sig over raw digest |

### Kernel

| Site | Role | Disposition |
|------|------|-------------|
| `security/signature.go:128` | addon bundle, raw sha256 digest, multi-key | stays (allowed); later delegates to signedenvelope |
| `licensing/sigv2/envelope.go:272,300`, `keyset.go:154` | `asteby1.` envelope, legacy token, TUF root | stays; it is the primitive signedenvelope builds on (allowed by the gate) |
| `hub/revocations.go:135` | revocation list (client side) | to migrate (baseline in the gate) |
| `installer/installer.go:591` | comment only | none |

### Classification by format

* Bundles of addons: ops 2, 6, 7, 9; kernel `security`. Four different
  message shapes for the same artifact (raw digest vs hex string vs dir digest).
* Licences/tokens: ops 10, 13; hub 6, 7, 8, 9 (legacy `b64.b64`), hub 7 and
  kernel sigv2 (`asteby1.`).
* Hub orders: ops 14; hub 5 (prefix domain, the only legacy format with domain
  separation).
* Handshake / device PoP (per-device key, not a trust anchor): ops 5; hub 1, 3,
  4; hub 12 and 11 (developer key).
* Autoupdate / releases: ops 1, 3, 4.
* Edge: ops 4, 5, 7.
* Bootstrap, brand pack, hub control, revocations: ops 8, 11, 12; hub 2, 10;
  kernel `hub/revocations.go`.

### Findings from the inventory

1. Bundle signatures (ops 2, 9), release signatures (ops 1, 3) and the order
   counter-signature (ops 6, 7) use the SAME marketplace key and a bare
   digest as message: a signature is valid wherever the digest matches,
   whatever the purpose. Only orders, bootstrap, brand pack and the edge
   manifest carry a domain prefix.
2. Hub 7 duplicates `licensing/sigv2` instead of importing it.
3. Typed errors differ everywhere (`errors.New` strings, `errHubSig`, ...);
   callers cannot branch on "expired" vs "bad signature" vs "unknown key".

## 2. API (`github.com/asteby/metacore-kernel/security/signedenvelope`)

Nothing about the deployed formats changes. The native envelope is the
existing sigv2 `asteby1.` format (header `{kid,dom,alg}`, message
`dom 0x0A header "." payload`); the package adds purpose-scoped keys, windows
and typed errors on top, plus adapters for the legacy formats.

```go
type Purpose string  // PurposeGrant, Lease, Release, Revocations, State, Bundle,
                     // Order, Bootstrap, BrandPack, HubControl, Enter, EdgeUpdate
type Key struct { ID string; Public ed25519.PublicKey; Purposes []Purpose
                  NotBefore, NotAfter *time.Time; Revoked bool }
func NewKeySet(keys ...Key) (*KeySet, error)      // fail closed: sizes, dup ids, purposes required
func ParseHexKeys(csv string, purposes ...Purpose) ([]Key, error)

type Verifier struct { Keys *KeySet; Policy Policy; Now func() time.Time }
type Policy struct { RequireNative bool; Window WindowFunc; Leeway time.Duration }

func (v *Verifier) Open(want Purpose, token string) (*Result, error)
    // "asteby1." envelope, or the legacy "b64(claims).b64(sig)" token (unless RequireNative)
func (v *Verifier) VerifyLegacy(p Purpose, s Scheme, input, sig []byte) (*Result, error)
func VerifyDetached(pub ed25519.PublicKey, msg, sig []byte) error   // PoP with a per-device key
func Seal(priv, kid, p, payload) (string, error)                    // wraps sigv2.Seal
func UnixWindow(nbfField, expField string) WindowFunc

// Schemes reproduce the deployed bare-signature formats byte for byte:
SchemeMessage            // sig over input as is (token claims, a digest passed in)
SchemeSHA256Raw          // sig over the 32 raw bytes of sha256(input)
SchemeSHA256Hex          // sig over the hex string of sha256(input)
SchemePrefixed(prefix)   // sig over prefix||input        (orders, bootstrap)
SchemePrefixedSHA256(p)  // sig over sha256(prefix||input) (brand packs)
```

Errors (`errors.Is`): `ErrMalformed`, `ErrBadSignature`, `ErrUnknownKey`,
`ErrWrongPurpose`, `ErrKeyPurpose`, `ErrKeyNotActive`, `ErrDowngrade`,
`ErrUnsupportedAlg`, `ErrExpired`, `ErrNotYetValid`, `ErrNoKeys`,
`ErrInvalidConfig`.

Security properties (all tested, see `signedenvelope_test.go`):

* Cross-purpose replay: a native token sealed for `grant` opened as `bundle`
  fails with `ErrWrongPurpose` BEFORE any key lookup; rewriting `dom` in the
  header breaks the signature; a key not declared for a purpose is never
  tried (`ErrKeyPurpose` / `ErrNoKeys`), which is the only protection the
  legacy bare formats can have.
* Truncated or extended signatures, truncated tokens, tampered payloads, wrong
  key material, unknown kid, expired / not-yet-active / revoked keys, empty key
  set, key with no declared purposes: all rejected.
* Rotation: several live keys per purpose; any may verify.
* Golden vectors with a fixed seed pin the deployed formats (raw digest, hex
  digest, order prefix, brand pack, legacy token, native envelope). If a vector
  fails, a deployed format changed.
* Downgrade: `Policy.RequireNative` refuses legacy formats; use one Verifier per
  purpose and flip it when the issuer emits `asteby1.`.

Caveat: legacy schemes cannot have domain separation. Bundle and release
signatures made with ONE key are interchangeable whenever the digest matches.
The migration therefore also asks for DISTINCT keys per purpose (ops 1/3 vs
2/6/7/9) before the native envelope is enforced.

## 3. Migration plan (phase 2, ops and hub; one PR per repo)

Order: kernel releases this package, ops/hub bump, then per row. No signing
side changes in step 1: every row is a drop-in replacement of the verify call.

### Ops

| Site | Target API | Notes |
|------|-----------|-------|
| 1 appliance self-update | `VerifyLegacy(PurposeRelease, SchemeMessage, digest, sig)` | later `Open(PurposeRelease)` when the pipeline emits native |
| 2, 9 addon bundle | `VerifyLegacy(PurposeBundle, SchemeMessage, []byte(hexDigest), sig)` | hex-string shape; 9 should move to `security.VerifyBundle` semantics |
| 3 asteby-update | `VerifyLegacy(PurposeRelease, SchemeMessage, []byte(hexDigest), sig)` | needs its own key (finding 1) |
| 4 edge update manifest | `VerifyLegacy(PurposeEdgeUpdate, SchemeMessage, SigningMessage(...), sig)` | message already domain-prefixed |
| 5 devauth | `VerifyDetached(pub, msg, sig)` | per-device key |
| 6, 7 bundle counter-sig | `VerifyLegacy(PurposeBundle, SchemeMessage, sum[:], sig)` | |
| 8 bootstrap | `VerifyLegacy(PurposeBootstrap, SchemePrefixed("asteby/act-bootstrap/v1\n"), doc, sig)` | |
| 10 applicense | `Open(PurposeGrant, token)` + `UnixWindow` | accepts legacy and `asteby1.` |
| 11 brandpack | `VerifyLegacy(PurposeBrandPack, SchemePrefixedSHA256(Domain), pack, sig)` | |
| 12 hubdir command | `VerifyLegacy(PurposeHubControl, SchemeMessage, sha256(body+"\n"+ts), sig)` | |
| 13 enter token | `Open(PurposeEnter, token)` | |
| 14 hubordersclient | `VerifyLegacy(PurposeOrder, SchemePrefixed("asteby-order-v1\x00"), raw, sig)` | |

### Hub

| Site | Target API | Notes |
|------|-----------|-------|
| 1, 3, 4 PoP | `VerifyDetached` | |
| 2 bootstrap | `VerifyLegacy(PurposeBootstrap, SchemePrefixed(...))` | |
| 5 orders_sign | `VerifyLegacy(PurposeOrder, SchemePrefixed(...))` | |
| 6, 9 licences | `Open(PurposeGrant)` | |
| 7 `license/envelope.go` | delete the copy, use `Open(PurposeGrant/Lease)` | removes the duplicated sigv2 |
| 8 instancelicense | `Open(PurposeGrant)` without `Window` | signature OK but possibly expired: `ErrExpired` is reported only when a Window is configured |
| 10 revocations | `VerifyLegacy(PurposeRevocations, SchemeMessage, canonicalPayload, sig)` | then sigv2 `DomRevocations` + Tracker |
| 11, 12 signer | `VerifyDetached` | developer key |

### Kernel

`security.VerifyBundle` calls `signedenvelope.VerifyLegacy(PurposeBundle, SchemeSHA256Raw, b.Raw, sig)`;
`hub/revocations.go` migrates likewise (multiple candidate messages stay).

### Sequencing, safety

1. Migrate rows with golden vectors from the deployed repos (each repo adds a
   test that runs a real, previously issued artifact through the new call).
2. Per purpose, issue distinct keys, then publish native tokens next to legacy
   ones, then set `RequireNative` for that purpose.
3. Never re-issue a licence/bundle to migrate: all rows above verify what is
   already out there.

## 4. CI gate

`scripts/check-ed25519-verify.sh` fails when `ed25519.Verify(` appears in
non-test Go code outside `security/` and `licensing/sigv2/`. It supports a
shrink-only baseline (`scripts/ed25519-verify-baseline.txt`; stale entries also
fail). Proposed wiring:

```yaml
# kernel .github/workflows/ci.yml
- name: ed25519.Verify gate
  run: scripts/check-ed25519-verify.sh --baseline scripts/ed25519-verify-baseline.txt
# ops and hub (copy the script, or fetch it from the kernel release):
- run: scripts/check-ed25519-verify.sh --allow '^$' --baseline scripts/ed25519-verify-baseline.txt
```

Ops/hub baselines start with the sites in section 1 and lose a line per
migrated row; the gate is switched to no baseline when the list is empty.
