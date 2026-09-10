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
8. Addons: package Baileys under `connector_whatsapp`.
9. Migration: import sessions through secret broker; canary one tenant.
10. Hub: host-profile filtering by OS/arch/native-runtime capability.

## Non-goals

- Running arbitrary binaries in-process.
- Docker socket access from addons.
- Raw environment secrets.
- Allowing native services in untrusted community tiers by default.
- Replacing WASM for ordinary business logic.
