# Capabilities enforcement

Every privileged operation an addon attempts (DB read/write, HTTP fetch, event
emit/subscribe) is checked by `security.Enforcer` against the capabilities the
addon declared in its manifest.

## Default: ENFORCE (BREAKING since the D5 hardening)

`security.NewEnforcer` starts in `ModeEnforce`. A call not covered by a
declared capability is logged (`metacore.capability.violation`) **and**
rejected; callers map the error to HTTP 403. An unknown addon is also denied.

Previously the default was `ModeShadow` (log only). Hosts whose addons make
undeclared calls will now see those calls fail.

## Opting back into shadow

```bash
METACORE_ENFORCE=shadow   # also: 0, false, no, off (case-insensitive)
```

Shadow logs every violation but lets the call proceed. Use it only as a
temporary migration aid while fixing manifests.

Any other value (unset, empty, `1`, `true`, `enforce`, or a typo) enforces:
the parser fails closed.

## Migrating

1. Before upgrading, run the previous kernel with `METACORE_ENFORCE=shadow` and
   collect `metacore.capability.violation` lines (or wire
   `Enforcer.OnViolation` to a metric).
2. Declare the missing capabilities in each addon manifest, or fix the addon.
3. Upgrade the kernel; the default now enforces. Keep `METACORE_ENFORCE=shadow`
   only for addons you cannot fix yet, and remove it afterwards.

The mode is still switchable at runtime with `Enforcer.SetMode`.
See also [permissions.md](permissions.md).
