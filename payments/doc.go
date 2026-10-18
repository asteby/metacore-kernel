// Package payments is the platform payments standard: the canonical Payment,
// Refund and PaymentEvent records, the normalized lifecycle with its valid
// transitions, the document-level charge hold and the idempotent replay rule.
// The provider-facing contracts (commerce.payments.*) live in package
// capability; this package is what every channel (POS, cash desk, kiosk,
// customer display, storefront, marketplace) and the payments ledger agree on
// about the records those contracts produce. See docs/RFC-PAYMENTS-STANDARD.md.
//
// Rules the package encodes:
//
//   - Money is an exact decimal string next to an ISO 4217 currency; it is
//     parsed with math/big, never with float64.
//   - The provider is the source of truth. A status is applied only after it
//     was re-read from the provider (commerce.payments.status); Observe
//     classifies such a re-read against the stored status (apply, ignore a
//     stale or duplicated update, or flag an anomaly such as money captured
//     after the payment was given up). Local commands (cancel requested, cash
//     counted) must follow the transition table exactly (Transition).
//   - An update is applied once: PaymentEvent.DedupeKey is unique per
//     provider observation.
//   - A document is charged by one point of sale at a time (Hold), and a
//     retried charge of an already-paid document is a replay, never a second
//     payment (CheckReplay).
//
// The package is pure Go (no database, no host): hosts persist the records,
// guests replicate the rules in their own language when they cannot import it.
package payments
