# CONF-PAYMENTS@1: conformance suite for payments providers

Package `payments/conformance` is the reusable suite a payments provider
(mercadopago, clip, stripe, a terminal ...) runs against its sandbox or
simulator. It uses only the existing contracts (`commerce.payments.session`,
`.confirm`, `.status`, `.refund`) and the rules of package `payments`; there is
no manifest schema change.

`payments/conformance/refsim` is the reference provider: an in-memory simulator
that passes every case. `conformance_test.go` also proves that a provider with
one defect (no signature check, no idempotency, float totals, outage reported as
`not_found` ...) fails the matching case.

## How an addon runs it in CI

The addon (or its simulator package) adds one Go test and runs it in the
normal `go test` step of its CI:

```go
func TestConformancePayments(t *testing.T) {
    sb := newSandbox(t) // talks to the provider SANDBOX only
    conformance.RunT(t, conformance.Target{
        Name:    "acme",
        Invoke:  sb.Invoke,  // func(ctx, "commerce.payments.session", input) (json.RawMessage, error)
        Sandbox: sb,         // Settle / Forge / Inject, see below
    }, conformance.Options{Strict: true})
}
```

`go test ./... -run TestConformancePayments -v` prints one subtest per case id
(`SES-01` ... `IND-03`) plus a table. For a machine-readable artifact use
`conformance.Run(...).JSON()` (`Report{suite, provider, run_id, cases[]}`).
A host-side `capability.Invoker` is adapted with `conformance.FromCapability`.

What the provider supplies:

- `Invoker`: calls the provider with the contract input (the same payload the
  host sends). Errors: `capability.ErrTimeout` / `ErrIndeterminate` /
  `context.DeadlineExceeded` for an unknown outcome, `*capability.CodedError`
  for typed errors (`not_found`), anything else is `provider_error`.
- `Sandbox`: `Settle(session, status)` makes the buyer finish a session and
  returns the signed webhook; `Forge(w)` alters a webhook keeping its
  signature; `Inject(fault)` arms `FaultSessionLostResponse`,
  `FaultRefundLostResponse` or `FaultProviderDown` for the next call (return
  `ErrFaultUnsupported` when the sandbox cannot; those cases are skipped, and
  fail under `Strict`).

`Options.Waive` exempts a case with a written reason (reported in the output),
`Options.Only` runs a subset by id prefix. The suite refuses to certify a
provider whose session does not report `test_mode=true`.

## Cases

| Id | What it proves |
|---|---|
| SES-01..05 | contract-shaped session, never `paid` at creation, same `idempotency_key` => same session, key reuse with another amount never makes a new session, distinct orders distinct sessions, sandbox (`test_mode=true`) |
| MON-01..05 | amounts with more decimals than the currency rejected (10.005 MXN, 10.50 JPY); 0, negative, `1e3`, `1,000.00`, `.5`, empty rejected; JPY whole units; captured amount/currency/order_ref equal the session's and quantized; refunds sum exactly (0.10 + 0.20 = 0.30, status `refunded`, no float drift) |
| WH-01..04 | valid signed webhook confirms and the ledger rule (`payments.ObserveOutcome`) applies it; body altered after signing rejected; unsigned rejected; replay answered with the same dedupe id and ignored by `ObserveOutcome` |
| ST-01..03 | `status` agrees with the confirmed webhook by `provider_ref` and `order_ref`; unknown reference is an error; `failed` / `expired` are final, reachable from `pending`, stable on re-read |
| RF-01..05 | partial refund (output, exact amount, `partially_refunded`); idempotent on its key (also with another amount); over-refund rejected, also after a full refund; full refund => `refunded`; uncaptured payment cannot be refunded |
| IND-01..03 | lost session/refund response is an indeterminate error (never a masked answer or `failed`), retry with the same key converges on one session / one refund; status during an outage is an error, never `paid`, a terminal status or `not_found` (only authoritative lookups may say `not_found`) |

Rule behind IND: an indeterminate outcome stays `pending`; it is resolved by
retrying with the same key or re-reading the provider, never assumed.

## Not covered yet

- `already_paid` with a second key and no cash-out in mixed payments are
  settlement (`payments.Settle`) rules of the host, not provider behavior.
- `commerce.payments.cancel`, terminals and `refund.status` have no case yet
  (suite version stays `@1`; new cases are additive, behavior changes bump it).
- There is no `metacore` CLI in this repository (`cmd/` is empty), so there is
  no `metacore conformance` subcommand. When the CLI lands it wraps
  `conformance.Run` and `Report.JSON`.
