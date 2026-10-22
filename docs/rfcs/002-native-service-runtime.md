# RFC 002 — Native service runtime for addon sidecars

Status: foundation accepted; Module Contract v3, verified artifacts, installer,
host lifecycle and the v1 operation envelope are implemented.

## Problem

Some integrations require a long-lived native runtime and cannot fit the WASM
request model. Baileys maintains WhatsApp sessions, sockets and browser crypto;
forcing it into the Ops process or keeping the Link backend alive would recreate
host coupling.

## Decision

Kernel will support signed addon-native services as isolated sidecars. A native
service is not an arbitrary post-install script. It is a declarative desired
state reconciled through `runtime/native.Supervisor`.

The first contract intentionally separates definition from execution:

- `runtime/native.Spec` validates entrypoint, scope, health, limits, network and
  opaque secret mounts;
- `Supervisor` is host-neutral and idempotent;
- Ops will implement it with rootless containers/systemd transient units;
- Hub bundles remain signed and content-addressed;
- Kernel installer invokes the supervisor only after trust, entitlement and
  compatibility gates.

## Security invariants

1. Entrypoint is a clean relative path inside the verified artifact.
2. No shell interpolation and no lifecycle scripts.
3. Read-only root filesystem; writable state uses an addon-scoped volume.
4. Non-root UID, dropped capabilities, no host PID/IPC namespace.
5. Deny-by-default egress; v1 accepts only exact host/host:port targets.
6. Secrets are broker handles mounted below `/run/secrets`, never manifest/env
   values.
7. Health and resource limits are mandatory.
8. Installation disable stops delivery and process eligibility.
9. Every operation carries tenant, installation, actor and trace envelopes.
10. Logs are structured and redacted at the supervisor boundary.

## Local control protocol

Every sidecar speaks `metacore.native/v1` over an Ops-assigned Unix HTTP
socket. Kernel reserves a small environment envelope containing only the
socket path, an opaque token-file path, installation ID, organization ID and
addon key. Manifests cannot add environment variables or choose host paths.
Health checks use the declared URL path over this authenticated socket, so no
fixed TCP port is exposed and concurrent installations cannot collide.

The data plane uses exactly `POST /v1/operations`. The host creates the
`runtime/native.Invocation` envelope and supplies organization, installation,
addon, actor and trace context from trusted runtime state; callers cannot put
tenant identity in a URL or override it in input. Services respond with the
closed `InvocationResult` success/error union. Both directions are capped at
4 MiB. A future streaming protocol will handle large media without weakening
this control channel.

## Event plane: sidecar → host

`POST /v1/operations` only lets the host call into a sidecar. Some
integrations (an inbound WhatsApp message, a connection-state change) need
the opposite direction. `runtime/native/events.go` defines that reverse
channel as a second, symmetric leg of the same `metacore.native/v1`
protocol rather than a bespoke bridge per addon:

- The supervisor provisions a **second** `unix_http` socket per
  installation, scoped 1:1 to it, at `EnvEventSocket` with its own one-time
  bearer token at `EnvEventTokenFile` — assigned exactly like
  `EnvSocket`/`EnvTokenFile` for operations. Manifests cannot choose or see
  either path.
- The sidecar `POST`s a `native.Event` to `EventsPath` (`/v1/events`) on
  that socket. **`Event` carries no organization_id/installation_id/
  addon_key.** The host resolves that trusted context from which
  authenticated socket/token the request arrived on, exactly as it already
  authors `InvocationContext` itself for operations instead of trusting
  addon input. A sidecar cannot claim an identity it doesn't hold.
- Payload is capped at `MaxEventPayloadBytes` (4 MiB, same limit as
  operations) and must be valid JSON; oversized or malformed bodies are
  rejected before any queuing.
- `type` is a dot-namespaced event name (`whatsapp.message.received`);
  `trace_id` is mandatory and must be propagated end-to-end from sidecar to
  the canonical event bus to the downstream consumer.
- `idempotency_key` is **mandatory** (unlike the optional one on
  operations, which are host-initiated). The host is the deduplication
  authority: replaying the same `(installation, idempotency_key)` returns
  `accepted:true, duplicate:true` without reprocessing, so an at-least-once
  sidecar retry after a dropped ack never double-publishes.
- Backpressure is a first-class, non-fatal outcome: a host that cannot keep
  up responds `429` with `Retry-After` and `EventAck{accepted:false,
  error:{code:"backpressure", retryable:true}}`. It must never block the
  connection indefinitely or tear down the sidecar to apply backpressure.

Kernel's scope stops at this contract (types, validation, wire shape,
env envelope). The host-side listener, queue, dedup store and canonical-bus
publisher are Ops' responsibility, the same split already used for the
operations dispatcher.

## Ingress plane: public HTTP → sidecar

Operations and events are private host↔sidecar channels. Some services must
also answer public HTTP (a server-side storefront renderer serving every
tenant's shop by domain). `runtime/native/ingress.go` defines that third leg
generically; nothing in it is storefront-specific.

Manifest shape (optional; absent means the service never receives public
traffic):

```jsonc
"ingress": {
  "kind": "http",            // only value in v1
  "path_prefix": "/",        // optional, default "/"; clean absolute path
  "max_body_bytes": 10485760, // optional, default 10 MiB, max 64 MiB
  "timeout": "30s"           // optional, default 30s, max 5m (response headers)
}
```

Allowed for both `scope: instance` (one service serving many tenants, which
resolves the tenant from the forwarded host) and `scope: installation`.

Contract:

- The supervisor assigns a **third** unix socket per service and exports it
  as `EnvIngressSocket` (`METACORE_NATIVE_INGRESS_SOCKET`), reserved exactly
  like `EnvSocket`. The sidecar serves plain HTTP/1.1 on it. It never binds a
  TCP port, and the manifest cannot choose or see the path.
- `runtime/native.IngressProxy` is the host-neutral reverse proxy every host
  reuses (`NewIngressProxy(socketPath, ingress, IngressProxyOptions)`, an
  `http.Handler`). Hosts must not re-implement the header discipline.
- Requests outside `path_prefix` (segment boundary) get 404; the path is
  forwarded unchanged and the original `Host` is preserved.
- Error mapping: declared or streamed body over the cap → 413; no response
  headers within `timeout` → 504; dial/transport failure → 502; protocol
  upgrade (WebSocket) → 501. Once headers arrive the response is streamed
  and flushed immediately.

### Security invariants

1. **Host-only exposure.** The ingress socket lives in the host-private
   socket directory; only the host dials it. No TCP listener, no port
   allocation, no collision between installations.
2. **The host decides routing.** Which public hosts (verified tenant domains,
   platform subdomains) reach a service is host state, never manifest input.
   The manifest can only narrow the path prefix.
3. **Hop-by-hop stripping** in both directions (`Connection` and every header
   it lists, `Keep-Alive`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`,
   `Proxy-*`).
4. **Forwarded headers are host-authored.** Client `Forwarded`,
   `X-Forwarded-*`, `X-Real-Ip`, `X-Request-Id`, every `X-Metacore-*` header
   and every host-declared trusted header (`TrustedHeaders`, e.g.
   `X-Storefront-Domain`) are dropped. The proxy then sets
   `X-Forwarded-For` (client IP as seen by the host, not appended to a client
   value), `X-Forwarded-Host`, `X-Forwarded-Proto` and runs the host's
   `SetTrustedHeaders` hook.
5. **Request id.** Every request carries a host-generated `X-Request-Id`
   upstream, echoed (once) on the response and on proxy error responses.
6. **Bounded.** Body cap and response-header timeout always apply, with
   defaults when the manifest omits them.
7. **No ingress without health.** A host routes traffic only while
   `Status.AcceptsIngress()` (state `healthy`); otherwise it answers 503
   itself without dialing, and a disabled installation answers 503 for its
   hosts only.

## Baileys target topology

`connector_whatsapp` owns the native service. `link_inbox` owns conversations
and messages. They communicate through declared operations/events, never direct
table access:

```text
WhatsApp network <-> Baileys sidecar
                       |
             signed local operation channel
                       |
          connector_whatsapp operations/events
                       |
                  link_inbox
```

Recommended scope is `instance`: one supervised connector service can host many
tenant sessions while Kernel issues tenant-scoped credentials and envelopes.
High-isolation deployments may choose `installation` scope.

## TCP listeners and port-scoped egress (mail-platform wave 0)

Some services speak a non-HTTP protocol straight to the internet (a mail server
on 25/465/587/993). Two additive extensions of the same contract cover them;
nothing changes for specs that do not use them.

### `ingress.listeners` (kind `tcp`, or alongside `http`)

```json
"ingress": { "kind": "tcp", "listeners": [
  { "name": "smtp", "port": 25, "proxy_protocol": "v2" },
  { "name": "submission", "port": 587, "proxy_protocol": "v2" },
  { "name": "imaps", "port": 993, "proxy_protocol": "v2" } ] }
```

- `kind: "tcp"` carries only listeners (no `path_prefix`/`max_body_bytes`/
  `timeout`, at least one listener). `kind: "http"` may also carry them; only an
  http ingress is mounted behind `IngressProxy` (`Ingress.HTTPEnabled()`).
- `name` `^[a-z][a-z0-9_]{0,31}$`, `port` is the PUBLIC TCP port (1-65535),
  unique per service; at most 16. `proxy_protocol`: `""`, `"v1"` or `"v2"`.
- **The sidecar never binds the public port.** The supervisor assigns each
  listener a loopback address and exports it as
  `METACORE_NATIVE_LISTENER_<NAME>` (`native.ListenerEnv(name)`, reserved like
  the other env vars); a host-side L4 forwarder (systemd socket, nftables
  redirect, userspace proxy) maps the public port to it. This is what lets a
  rootless sidecar serve port 25.
- With `proxy_protocol` the forwarder prepends a HAProxy PROXY header so the
  sidecar sees the real client address (SPF, greylisting, abuse). The listener
  must then be reachable only from the forwarder (loopback).
- **The host may refuse a port.** Conflicts with another service on the machine,
  firewall policy and tenant limits are host decisions: refuse the install or
  mark the service blocked; never rebind or stop a foreign service.

### `network.egress` grammar

| Entry | Meaning |
|---|---|
| `host` | exact hostname |
| `host:port` | exact hostname and TCP port |
| `*:<port>` | any PUBLIC host on one port, **only** `25 110 143 465 587 993 995` |

Everything else stays forbidden: a bare `*`, `*.example.com`, `mail.*`,
`*:443`, `*:*`, ranges and CIDR (`ParseEgress` returns the reason). The
wildcard exists because MX delivery and tenant-configured IMAP/SMTP servers have
destinations that cannot be enumerated in a signed manifest; the closed port
list (`WildcardEgressPorts`) keeps it from becoming "reach anything". Widening
it is a kernel release.

**Wildcard does not lift SSRF defences.** Enforcers must resolve the name and
still refuse loopback, private, link-local, CGNAT and metadata addresses
(`netguard.IsBlockedIP`) on the address the socket actually connects to.
`NetworkPolicy.Allows(host, port)` is the reference decision function for the
manifest grammar.

## Delivery sequence

1. Complete: portable spec, validation and supervisor port.
2. Complete: Manifest v3 `runtime.native_service` schema and projection.
3. Complete: OS/arch artifact descriptors, digest and SBOM verification.
4. Complete: installer ensure/health and full enable/disable/uninstall lifecycle.
5. Complete: Ops rootless supervisor and authenticated local health transport.
6. Complete: versioned operation request/response envelope.
7. Ops: operation dispatcher over the authenticated Unix socket.
8. Complete: versioned sidecar -> host event envelope (`/v1/events`,
   `runtime/native/events.go`) — kernel side only.
9. Ops: event-plane receiver, dedup store and canonical-bus publisher.
10. Addons: package Baileys under `connector_whatsapp`.
11. Migration: import sessions through secret broker; canary one tenant.
12. Hub: host-profile filtering by OS/arch/native-runtime capability.
13. Complete: ingress plane contract and reusable `IngressProxy` — kernel side.
14. Ops: assign `EnvIngressSocket`, map verified hosts to services, mount the
    proxy behind TLS.
15. Complete (kernel): TCP `ingress.listeners` + port-scoped egress contract.
16. Ops: L4 forwarder for listeners (`METACORE_NATIVE_LISTENER_<NAME>`, PROXY
    protocol), port-conflict admission, egress enforcement with `Allows` + SSRF.

## Non-goals

- Running arbitrary binaries in-process.
- Docker socket access from addons.
- Raw environment secrets.
- Allowing native services in untrusted community tiers by default.
- Replacing WASM for ordinary business logic.
