// Package inbound is the generic receiving side of RFC 0003 (F4): one HTTP
// endpoint, POST /api/inbound/{org}/{contract}, that turns a signed external
// event into a create/update/upsert on a dynamic model (or a call to an
// already registered command) driven only by DATA.
//
// A new event type is a Contract (key, version, JSON Schema, route + mapping,
// idempotency template, auth profile) loaded into a Registry; no route and no
// code per type. See docs/inbound-events.md for the full guide.
//
// Request pipeline (each step fails closed with the uniform error envelope):
//
//  1. pre-auth rate limit (per client IP, optional)
//  2. path -> organization (uuid) + contract (+ optional X-Contract-Version)
//  3. body size limit (default 1 MiB)
//  4. Verifier of the contract's auth profile (401 on any failure)
//  5. per-organization rate limit (optional)
//  6. JSON decode + schema validation (422, no payload values echoed)
//  7. event key from the idempotency template; reservation in the receipt log
//     (duplicate -> 200 "replayed"; same key, other body -> 409)
//  8. declarative mapping, then Executor (model routes) or CommandRegistry
//     or, when the contract declares Actions (RFC 0003 §2.4), the chain in
//     chain.go: per-action progress in the receipt, resumable, origin-marked
//  9. receipt finished as applied / failed; the caller gets applied|error
//
// The organization always comes from the authenticated route, never from the
// payload; model routes run through dynamic.Service as a system principal of
// that organization, so tenant scoping, validation, hooks and events apply.
//
// Wiring:
//
//	reg := inbound.NewRegistry()
//	_ = reg.LoadJSON(contractsJSON)
//	rcpt, _ := inbound.NewGormReceiptStore(db)
//	h, err := inbound.New(inbound.Config{
//	    Contracts: reg,
//	    Verifiers: map[string]inbound.Verifier{"hmac_canonical_v1": verifier},
//	    Receipts:  rcpt,
//	    Executor:  &inbound.DynamicExecutor{Service: dyn},
//	})
//	mux.Handle("POST /api/inbound/", h)
//
// Verifier is the minimal authentication port; the webhooks package supplies
// the production profiles (hmac_simple, hmac_canonical_v1, bearer,
// bearer+hmac_canonical_v1). HMACCanonicalV1 here is a self-contained
// implementation used by this package's tests and usable as is.
package inbound
