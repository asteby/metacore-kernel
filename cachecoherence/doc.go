// Package cachecoherence is the kernel's single answer to "a cache somewhere
// still holds the shape of the world before the addon changed".
//
// # Problem
//
// Installing, upgrading, enabling, disabling or uninstalling an addon changes
// things that many layers derive state from: the database schema (and with it
// every prepared statement / cached plan), the manifest registry, the RBAC
// capability cache, the capability catalog, the API gateway routes, the
// navigation tree, the dynamic-model metadata, the wasm module. Each layer
// used to cache on its own and be invalidated (or not) on its own; the ones
// that were forgotten kept serving the old world until the process restarted.
//
// # Model
//
//   - Epoch. A monotonic counter per scope: one global, one per organization,
//     one per addon. [Registry.Epoch] returns the triple for an (org, addon)
//     pair and [Epoch.Token] renders it as an opaque string suitable for an
//     ETag, an X-Cache-Epoch header or a cache key. A consumer that tags what
//     it cached with the token it was computed under can tell, by comparing,
//     whether it is stale. [Watcher] packages that comparison.
//   - Bump. [Bump] is the only API a mutation path has to call:
//     cachecoherence.Bump("addon.upgrade", WithOrg(org), WithAddon(key)).
//   - Invalidators. Layers register once with [Register], naming themselves
//     and giving an order. Bump runs them synchronously, in ascending order,
//     so dependencies are honoured (the prepared-statement pool is flushed
//     BEFORE anything re-reads the schema; derived read caches go next; the
//     layers that publish to browsers go last). One invalidator failing (or
//     panicking) is recorded and does not stop the rest: a half-flushed world
//     is worse than a flushed one with one error logged.
//   - Publication order. The new epoch becomes visible only AFTER every
//     invalidator ran. A consumer that sees the new token and refetches is
//     therefore guaranteed to read through clean caches; one that refetched
//     in the middle of the bump keeps the old token and is detected stale.
//
// # Limits
//
// The registry is per process. A multi-replica deployment must forward the
// event ([Registry.Subscribe]) and apply it on the peers with
// [Registry.ApplyRemote]; the kernel does not ship a transport. Epoch bases
// are derived from the process start time so a restart never yields a token
// that a pre-restart client can mistake for "unchanged".
package cachecoherence
