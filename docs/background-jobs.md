# Durable idempotency and background jobs

Two kernel primitives for apps that talk to the outside world (payment
gateways, ERPs, marketplaces, webhooks):

| Package        | Solves                                                            | Table                       |
| -------------- | ----------------------------------------------------------------- | --------------------------- |
| `idempotency/` | A retried or double-clicked POST must not create a second order   | `metacore_idempotency_keys` |
| `outbox/`      | A side effect must survive a crash, retry, and never run twice at once | `metacore_outbox_jobs`      |

Both target Postgres in production and also run on SQLite for tests.
Both create their own table idempotently at construction, under an advisory
lock. `idempotency.GormStoreOptions.SkipMigrate` and
`outbox.Config.SkipMigrate` opt out; `outbox.Schema` returns the DDL.

## Idempotency keys that survive replicas and restarts

```go
store, err := idempotency.NewGormStore(db, idempotency.GormStoreOptions{})
if err != nil {
    return err
}
defer store.Close() // stops the hourly cleanup of expired keys

r.Post("/checkout", idempotency.Middleware(idempotency.Config{
    Store:        store,
    UserKey:      func(c fiber.Ctx) string { return auth.GetUserID(c).String() },
    InFlightWait: 10 * time.Second, // duplicate waits for the first, then replays it
    LockTimeout:  time.Minute,      // > slowest checkout; a dead replica's claim expires after this
}), h.checkout)
```

Behaviour with a key the server has already seen:

| Situation                                  | Response                                                              |
| ------------------------------------------ | --------------------------------------------------------------------- |
| First request finished with 2xx            | Stored response replayed (`Idempotent-Replay: true`), handler not run |
| First request still running                | Waits up to `InFlightWait` and replays; else **409** `idempotency_key_in_flight`, `Retry-After: 1` |
| First request failed (error, panic, non-2xx) | Key released: the retry runs the handler                            |
| Store unreachable                          | **503**. The middleware fails closed and never double-executes     |

The in-flight guarantee comes from the optional `idempotency.Locker`
interface (`Reserve` / `Release`). `GormStore` implements it. Plain `Store`
implementations, such as `InMemoryStore` or custom Redis stores, keep the
old check-then-store behaviour.

With `host.App`, set `EnableIdempotencyKey: true, IdempotencyDurable: true`.

## Outbox jobs

```go
jobs, err := outbox.New(db, outbox.Config{Workers: 4})
if err != nil {
    return err
}
jobs.Register("erp.post_order", func(ctx context.Context, job outbox.Job) error {
    var p struct{ OrderID uuid.UUID `json:"order_id"` }
    if err := job.Decode(&p); err != nil {
        return outbox.Permanent(err) // dead-letter immediately
    }
    return erp.Post(ctx, p.OrderID) // error → retry with backoff
})
if err := jobs.Start(ctx); err != nil { // cancel ctx → graceful shutdown
    return err
}
```

Enqueue inside the business transaction, so the job exists if and only if
the order does:

```go
err := db.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(&order).Error; err != nil {
        return err
    }
    _, err := jobs.EnqueueTx(tx, "erp.post_order", map[string]any{"order_id": order.ID},
        outbox.WithOrg(order.OrgID),
        outbox.WithDedupeKey(order.ID.String()))
    return err
})
```

| Concern            | Behaviour                                                                                   |
| ------------------ | ------------------------------------------------------------------------------------------- |
| Concurrency        | `UPDATE … FROM (SELECT … FOR UPDATE SKIP LOCKED)`. No two workers on any replica claim the same job |
| Kinds              | A process only claims kinds it registered, so services can share the table                  |
| Retries            | Backoff is `BackoffBase·2^(n-1)` (default 10s, capped at 1h) with jitter. `RetryAfter(err, d)` overrides it |
| Dead-letter        | After `MaxAttempts` (default 10) or a `Permanent` error → `dead`. `Retry(id)` revives it with a fresh budget |
| Stuck jobs         | The lease (`LeaseTimeout`, default 5m) is renewed while the handler runs. After a crash the job is reclaimed once the lease expires. A job with no attempts left is dead-lettered |
| Fencing            | Each claim has a token. A worker that lost its lease cannot record a result, and its ctx is cancelled with `ErrLeaseLost` |
| Dedupe             | `WithDedupeKey` makes the key unique per kind among pending and running jobs. A duplicate returns the live job and `ErrDuplicate` |
| Shutdown           | Stops claiming and waits `ShutdownGrace` (30s). Then it cancels handler ctxs, and interrupted jobs get their attempt back |
| Retention          | Done jobs are deleted after 7 days (`RetainDone`). Dead jobs are kept (`RetainDead`) |
| Metrics            | `Config.Hooks` has `OnEnqueue`, `OnStart`, `OnSuccess`, `OnRetry` and `OnDead` |
| Admin API          | `outbox.NewHandler(svc, scope).Mount(r)` serves `GET /jobs`, `GET /jobs/:id` and `POST /jobs/:id/retry` |

Delivery is **at-least-once**. Make handlers idempotent: skip work already
done, and send `job.ID` or a business folio as the remote idempotency key.

With `host.App`, set `EnableOutbox: true` (and optionally `OutboxConfig`).
Then call `app.Outbox.Register(...)` and `app.Outbox.Start(ctx)`.

### Adopting it in an existing app (7leguas-ecommerce)

The following code replaces `idempotency.NewInMemoryStore(5000)` in
`backend/handlers.go`:

```go
checkoutIdempotency, err := idempotency.NewGormStore(s.db, idempotency.GormStoreOptions{})
// …
r.Post("/checkout", idempotency.Middleware(idempotency.Config{
    Store:        checkoutIdempotency,
    UserKey:      func(c fiber.Ctx) string { return kauth.GetUserID(c).String() },
    InFlightWait: 10 * time.Second,
}), s.handleCheckout)
```

The following code replaces `s.postOrderToERP(o)` and
`go s.meliSyncListings(o.Lines)` with jobs:

```go
s.jobs.Register("erp.post_order", func(ctx context.Context, j outbox.Job) error {
    var p struct{ OrderID uuid.UUID `json:"order_id"` }
    if err := j.Decode(&p); err != nil {
        return outbox.Permanent(err)
    }
    var o models.Order
    if err := s.db.Preload("Lines").First(&o, "id = ?", p.OrderID).Error; err != nil {
        return err
    }
    s.postOrderToERP(&o) // already skips posted/cancelled orders
    if o.ERPStatus != "posted" {
        return errors.New(o.ERPMessage) // retried with backoff, then dead-lettered
    }
    return nil
})
s.jobs.Register("meli.sync_listings", func(ctx context.Context, j outbox.Job) error {
    var p struct{ Lines []models.OrderLine `json:"lines"` }
    if err := j.Decode(&p); err != nil {
        return outbox.Permanent(err)
    }
    return s.meliSyncListingsErr(ctx, p.Lines) // variant that returns its error
})

// In the checkout transaction, next to the order insert:
s.jobs.EnqueueTx(tx, "erp.post_order", map[string]any{"order_id": o.ID},
    outbox.WithDedupeKey("erp:"+o.ID.String()))
s.jobs.EnqueueTx(tx, "meli.sync_listings", map[string]any{"lines": o.Lines})
```
