// Package outbox is a durable background-job queue for integrations: ERP
// postings, marketplace syncs, outbound webhooks — any side effect that
// must survive a crash, retry on failure and never run twice at once.
//
// Jobs live in the table `metacore_outbox_jobs` in the app's own database,
// so they can be enqueued inside the same transaction as the business rows
// that caused them (the transactional-outbox pattern): the order and its
// "post to ERP" job commit together or not at all.
//
//	jobs, err := outbox.New(db, outbox.Config{Workers: 4})
//	if err != nil {
//	    return err
//	}
//	jobs.Register("erp.post_order", func(ctx context.Context, job outbox.Job) error {
//	    var p struct{ OrderID uuid.UUID `json:"order_id"` }
//	    if err := job.Decode(&p); err != nil {
//	        return outbox.Permanent(err) // bad payload: dead-letter now
//	    }
//	    return erp.PostOrder(ctx, p.OrderID, job.ID.String()) // job.ID as remote idempotency key
//	})
//	if err := jobs.Start(ctx); err != nil { // cancel ctx → graceful shutdown
//	    return err
//	}
//	defer jobs.Shutdown(context.Background())
//
//	// Atomically with the order:
//	db.Transaction(func(tx *gorm.DB) error {
//	    if err := tx.Create(&order).Error; err != nil {
//	        return err
//	    }
//	    _, err := jobs.EnqueueTx(tx, "erp.post_order", map[string]any{"order_id": order.ID},
//	        outbox.WithOrg(order.OrgID), outbox.WithDedupeKey(order.ID.String()))
//	    return err
//	})
//
// # Semantics
//
//   - Claiming: each poll moves up to the number of free worker slots from
//     pending to running with a single UPDATE over a `SELECT … FOR UPDATE
//     SKIP LOCKED` (Postgres). Any number of workers on any number of
//     replicas never claim the same job twice. Workers only claim kinds
//     registered in their process.
//   - Leases: a claimed job carries locked_until = now + LeaseTimeout and a
//     per-claim token, renewed every LeaseTimeout/3 while the handler runs.
//     If the process dies, the job becomes claimable again once the lease
//     expires (stuck-job recovery). Result writes are fenced by the token,
//     so a worker that lost its lease cannot overwrite the new owner; its
//     handler context is cancelled with ErrLeaseLost.
//   - Retries: a handler error reschedules the job at now + backoff
//     (exponential with jitter, BackoffBase·2^(n-1) up to BackoffMax) or
//     at the delay given with RetryAfter. After MaxAttempts attempts — or
//     immediately for a Permanent error — the job is dead-lettered
//     (status dead) and kept for inspection until Retry.
//   - Delivery is at-least-once. A crash between the side effect and the
//     "done" write re-runs the handler, so make handlers idempotent;
//     job.ID is a stable key to pass to the remote system.
//   - Dedupe: WithDedupeKey makes (kind, key) unique among pending and
//     running jobs (partial unique index). A duplicate Enqueue returns the
//     live job and ErrDuplicate; the key is free again once that job is
//     done or dead.
//   - Shutdown: cancelling Start's context (or calling Shutdown) stops
//     claiming and waits for running jobs. Past the grace period their
//     contexts are cancelled and interrupted jobs go back to pending
//     without spending an attempt.
//   - Retention: done jobs are deleted after RetainDone (7 days); dead
//     jobs are kept forever unless RetainDead is set.
//
// Timestamps are written from the application clock, so replicas must be
// NTP-synced; skew only shifts lease expiry and run_at by the skew.
//
// # Operations
//
// List, Get and Retry back admin UIs; Handler exposes them over Fiber
// (optional, mount it behind your own admin guard). Hooks give metric
// callbacks (enqueue, start, success, retry, dead) without tying the
// package to a metrics backend.
//
// # Schema
//
// New runs Migrate (CREATE TABLE/INDEX IF NOT EXISTS under an advisory
// lock) unless Config.SkipMigrate; Schema returns the same DDL for hosts
// that manage migrations themselves. host.NewApp wires all of this when
// AppConfig.EnableOutbox is true and exposes the service as app.Outbox.
// Postgres is the production target; SQLite works for tests and
// single-node tools.
//
// # Stability
//
// New in this release and considered stable under the kernel's semver:
// Service's exported methods, Config, Job, Status, Filter, Hooks,
// HandlerFunc, the Enqueue options, the sentinel errors, Permanent,
// RetryAfter, Handler and the table layout. Adding Config fields, options
// or columns is a minor bump.
package outbox
