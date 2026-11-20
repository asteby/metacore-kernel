// Package signedenvelope is the single, general entry point for verifying
// Ed25519-signed material in the metacore ecosystem (kernel, ops, hub).
//
// WHY. Before this package every consumer re-implemented "decode, check
// sizes, ed25519.Verify" by hand (~30 call sites). Each copy picked its own
// message shape, its own error values, its own key handling, and most had NO
// domain separation: a signature minted for one purpose (a licence) could be
// presented where another one is expected (a bundle) whenever the message
// bytes happened to line up. This package centralises that logic.
//
// WHAT IT GIVES YOU.
//
//   - Purpose: a domain-separation tag. Every verification names the purpose
//     it expects; the native envelope signs the purpose inside the message, so
//     a token sealed for one purpose can never verify as another one.
//   - KeySet: keys with an id (kid), the purposes each key may sign, an
//     optional validity window and a revoked flag. Several keys may be live
//     at once, which is what makes rotation possible without downtime.
//   - Verifier.Open: verifies the native "asteby1." envelope (the sigv2
//     wire format, re-used verbatim; nothing new is invented) and, unless
//     Policy.RequireNative, the legacy two-part token "<b64 payload>.<b64 sig>".
//   - Optional validity window of the payload (not-before / expiry, with
//     leeway) through Options.Window.
//   - Verifier.VerifyLegacy: verifies the already deployed bare-signature
//     formats through named Schemes (raw digest, hex digest, prefixed
//     message, hashed prefixed message) so ops/hub can migrate their call
//     sites without re-issuing a single licence, bundle or order.
//   - VerifyDetached: for proof-of-possession signatures made with a
//     per-device key (not a trust anchor), with the same typed errors.
//   - Typed errors (errors.Is) for every failure class.
//
// WHAT IT DOES NOT DO. It does not change any deployed wire format, does not
// issue anything (apart from Seal, a thin wrapper used by tests/issuers) and
// uses nothing but crypto/ed25519 from the standard library. The legacy
// schemes carry NO domain separation on the wire (that is the defect they
// have); they are confined by key scoping instead (Key.Purposes) and are meant
// to be switched off per purpose (Policy.RequireNative) once the issuer
// emits the native envelope.
//
// SECURITY NOTES.
//   - Fail closed: an empty key set, a key with no declared purposes, an
//     expired/revoked key or a wrong-length signature never verify.
//   - Signatures are never "trimmed": decoders in the legacy schemes belong to
//     the caller; this package receives raw bytes.
//   - Verification order for the native envelope: size, shape, algorithm,
//     purpose (before any key lookup), key resolution (kid, purpose, window),
//     then the Ed25519 check over header+payload exactly as received.
package signedenvelope
