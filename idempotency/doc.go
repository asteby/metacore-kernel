// Package idempotency provides server-side replay caching for non-idempotent
// HTTP endpoints. Clients send an `Idempotency-Key` header (Stripe-style)
// and the kernel's middleware short-circuits duplicate requests with the
// stored response — guarantees retries from a flaky network never produce
// double-creates or double-imports.
//
// # Stores
//
// The middleware talks to a Store (Get/Put). Two implementations ship:
//
//   - InMemoryStore — goroutine-safe LRU+TTL for single-replica apps and
//     tests. Lost on restart; not shared between replicas.
//   - GormStore — durable table `metacore_idempotency_keys` (Postgres; SQLite
//     for tests). Shared by every replica, survives restarts, deletes
//     expired rows periodically. Use it for checkout, payments and any
//     multi-replica deployment.
//
// Wiring the durable store:
//
//	store, err := idempotency.NewGormStore(db, idempotency.GormStoreOptions{})
//	if err != nil {
//	    return err
//	}
//	defer store.Close()
//	api.Post("/checkout",
//	    idempotency.Middleware(idempotency.Config{Store: store, InFlightWait: 5 * time.Second}),
//	    h.checkout)
//
// # Concurrent duplicates
//
// A store that also implements the optional Locker interface (GormStore
// does) lets the middleware claim the key with a unique "pending" row
// BEFORE the handler runs. A second request with the same key that arrives
// while the first is still processing never executes the handler:
//
//   - it polls for up to Config.InFlightWait and replays the first
//     response as soon as it is stored, or
//   - it gets 409 Conflict with `Retry-After: 1` and code
//     `idempotency_key_in_flight` (immediately when InFlightWait is zero,
//     which matches Stripe).
//
// If the first request fails (handler error, panic, non-2xx status) its
// claim is released, so the client's retry runs the handler again. If the
// replica dies mid-request the claim expires after Config.LockTimeout
// (default 1m). If the store cannot be reached the middleware fails closed
// with 503 rather than risk a double execution.
//
// Stores without Locker (InMemoryStore, custom ones) keep the original
// check-then-store flow, where two truly simultaneous duplicates can both
// run the handler.
//
// # Stability
//
// Store, Locker, Reservation, Config, Middleware, InMemoryStore and
// GormStore are stable under the kernel's semver. Adding a method to Store
// or Locker is a major bump; new capabilities arrive as new optional
// interfaces detected by type assertion, as Locker did.
package idempotency
