# Durable delivery de-duplication (`support/dedupe`)

`contract.DeliveryDeduper` is the port a webhook receiver uses to drop a
delivery id it already handled. `contract.MemoryDeduper` is for tests and
single-process hosts. `support/dedupe` is the durable implementation for
production: it survives restarts and is atomic across replicas.

## Design decision

No new table. `idempotency.GormStore` already offers an atomic claim with a
lease (`Reserve`: one `INSERT ... ON CONFLICT DO UPDATE ... WHERE expired`),
which is exactly "first seen for ttl". A de-duplication id is a row of
`metacore_idempotency_keys` whose key is `dedupe:<namespace>:<id>`; the
prefix keeps it apart from the HTTP middleware's keys. Expired rows are
purged by the store's own cleanup goroutine (or `Deduper.Purge`).

Added to `idempotency.GormStore`: `CountLive(ctx, prefix)`, used only for the
optional capacity limit.

## Usage

```go
store, err := idempotency.NewGormStore(db, idempotency.GormStoreOptions{})
if err != nil { return err }
defer store.Close()

dd, err := dedupe.New(store, dedupe.Options{
    Namespace: "support-callback", // one per consumer; reuse the store
    // TTL:        0,              // 0 = the caller's ttl (contract.DedupeTTL, 7 days)
    // MaxEntries: 0,              // 0 = unbounded; >0 = ErrDedupeFull when reached
})

meta, err := contract.AcceptCallback(ctx, dd, secret, body, r.Header, time.Now(), 0)
if errors.Is(err, contract.ErrDuplicate) { /* answer 2xx */ }

// Processing failed after FirstSeen returned true: let the redelivery in.
dd.Forget(ctx, meta.DeliveryID)
// Processing succeeded: free the in-process token (the record stays).
dd.Confirm(meta.DeliveryID)
```

Semantics are identical to `MemoryDeduper` and enforced by one shared suite,
`support/contract/contracttest.RunDedupe`, which runs against both. Any new
implementation of the port should run it too.

Notes:
- A non-positive ttl records nothing and reports true (as `MemoryDeduper`).
- `MaxEntries` is checked after the claim; near the limit concurrent callers
  may be rejected slightly early (fail closed, the sender retries). It never
  stays over the limit.
- `Forget` needs the in-process token; after a restart the record simply
  expires with its TTL.

## For the OPS WP-06 agent

OPS `hubsupport.Deduper` (`Reserver`, `dedupeNamespace`, `tokens`, `Forget`,
`Confirm`) is the same idea and can be deleted in favour of the kernel type:

| OPS today | Kernel |
|---|---|
| `hubsupport.NewDeduper(store)` | `dedupe.New(store, dedupe.Options{Namespace: "support-callback"})` |
| `Deduper.FirstSeen/Forget/Confirm` | same names and signatures on `*dedupe.Deduper` |
| `dedupeNamespace = "support:callback:"` | key is now `dedupe:support-callback:<id>` |
| `Reserver` interface | `dedupe.Store` (adds `Cleanup`, `CountLive`; `*idempotency.GormStore` satisfies it) |
| `support_callback_seen` table | none |

Rows written under the old `support:callback:` prefix are not read by the new
key; they expire on their own TTL, so one extra delivery per in-flight id may
be processed once during the switch. Requires this kernel version (for
`GormStore.CountLive` and the `support/dedupe` package).

## Tests

`support/dedupe/gorm_test.go` runs the shared suite plus multi-replica
concurrency (several `GormStore`s on one database), namespaces, `Forget`,
`Purge`, and validation. SQLite (mattn driver, needs CGO) always; Postgres
when `TEST_POSTGRES_DSN` points to a scratch database, like `idempotency`.
