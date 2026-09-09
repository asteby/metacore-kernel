# RFC 002 — Native service runtime for addon sidecars

Status: foundation accepted in code; manifest wiring and host supervisor pending.

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

1. This PR: portable spec, validation and supervisor port.
2. Manifest v3: `runtime.native_service` schema and projection.
3. Bundle: OS/arch artifact descriptors, digest and SBOM verification.
4. Installer: journaled ensure/health/rollback integration.
5. Ops: rootless supervisor adapter and local authenticated transport.
6. Addons: package Baileys under `connector_whatsapp`.
7. Migration: import sessions through secret broker; canary one tenant.
8. Hub: host-profile filtering by OS/arch/native-runtime capability.

## Non-goals

- Running arbitrary binaries in-process.
- Docker socket access from addons.
- Raw environment secrets.
- Allowing native services in untrusted community tiers by default.
- Replacing WASM for ordinary business logic.
