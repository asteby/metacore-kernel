// Package contract is the single source of truth for the support escalation
// contract shared by the Hub, OPS instances, desk addons and external clients
// (for example the PHP client of a vertical product).
//
// It holds, with no I/O and no dependency outside the standard library:
//
//   - the wire types of POST /v1/support/escalate, /message, /link and
//     /link/ack and of the Hub -> instance callback events;
//   - the stable constants (events, error codes, link source/state, severity,
//     headers, versions);
//   - pure helpers: HMAC signing and verification of callbacks with a +-300 s
//     window (SignCallback, VerifyCallback), the link acknowledgement proof,
//     the ticket folio and message id validators, and the DeliveryDeduper port
//     (in-memory here; durable in support/dedupe) that receivers use to drop repeated
//     delivery ids.
//
// Versioning: see README.md. Within a contract version only optional fields
// are added; receivers must ignore unknown fields and unknown events.
package contract
