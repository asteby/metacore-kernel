// Package nativekit holds the primitives every native (compiled-in) module
// needs and that were being re-written per module:
//
//   - Error / Code: typed errors with stable, machine-readable codes the HTTP
//     layer and i18n can map without parsing messages.
//   - Run / Op: one database transaction per business operation, with
//     AfterCommit / OnRollback hooks so events and side effects fire only once
//     the data is durable.
//   - Once: idempotency in the database. A unique (org, scope, key) row is
//     claimed inside the operation's own transaction, so a retried request
//     replays the stored result and a concurrent duplicate blocks on the unique
//     index instead of double-applying.
//   - LockRows / SortedIDs: deterministic row-lock ordering so two operations
//     that touch the same rows in opposite order cannot deadlock.
//   - nativekit/dec: a comparable fixed-point decimal for balances.
//   - nativekit/ledger: the generic balance + movement ledger built on top.
//
// None of it knows about a vertical: modules bring their own tables and rules.
package nativekit
