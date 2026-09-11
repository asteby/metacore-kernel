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

## Non-goals

- Running arbitrary binaries in-process.
- Docker socket access from addons.
- Raw environment secrets.
- Allowing native services in untrusted community tiers by default.
- Replacing WASM for ordinary business logic.
