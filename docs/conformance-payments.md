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
        Name:     "acme",
        Invoke:   sb.Invoke, // func(ctx, "commerce.payments.session", input) (json.RawMessage, error)
        Sandbox:  sb,        // Settle / Age / Effects / Inject, see below
        Supports: conformance.Supports{}, // declared limits, see "Not applicable"
    }, conformance.Options{}) // strict by default
}
```

The same shape is compiled with the package (`example_test.go`), and
`TestDocMatchesAPI` fails if this doc names an API that no longer exists.


`go test ./... -run TestConformancePayments -v` prints one subtest per case id
plus a table. For a machine-readable artifact use `conformance.Run(...).JSON()`
(`Report{suite, provider, run_id, webhook_mode, only, waivers, aborted, cases[]}`).
A host-side `capability.Invoker` is adapted with `conformance.FromCapability`.

What the provider supplies:

- `Invoker`: calls the provider with the contract input. Errors:
  `capability.ErrTimeout` / `ErrIndeterminate` / `context.DeadlineExceeded` for
  an unknown outcome, `*capability.CodedError` for typed errors (`not_found`,
  `idempotency_conflict`), anything else is `provider_error`.
- `Sandbox`: `Settle(SettleSpec{SessionID, Status, Amount, Currency})` makes the
  buyer finish a session (for what was REALLY charged) and returns the signed
  webhook (status `pending` = a late notification that must not move a paid
  payment); `Age(w, by)` re-issues a webhook with an old timestamp;
  `Effects(order_ref)` counts the sessions and refunds that really exist on the
  provider side (ground truth for "a retry did not duplicate"); `Inject(fault)`
  arms `FaultSessionLostResponse`, `FaultRefundLostResponse` or
  `FaultProviderDown`. A method the sandbox cannot do returns `ErrUnsupported`.

## Verdict semantics

- Strict is the default: `Conforms()` needs no failed case AND no skipped case.
  A skip is a case that could not conclude (`ErrUnsupported`, a refund that
  stayed `pending` past `PollTimeout`). `Options.Lenient` tolerates skips but
  gives its own verdict, `conforms_lenient`, and `Conforms()` stays false: a
  sandbox that cannot count effects or inject faults is not certified.
- Preflight, not waivable and not filterable: before any case the provider must
  answer a session with `test_mode=true`, or the run is `aborted`. Every
  session, confirm and status output of every case is also checked
  (`test_mode=false` or `x_environment` other than sandbox fails the case).
- `Options.Only` makes the run partial: `Conforms()` is always false
  (`Verdict()` = `partial`). Use `go test -run` for local iteration.
- `Options.Waive` exempts case ids for a mandatory written reason; an unknown
  id, a blank reason, more than `MaxWaivers` (5) or any of the `CoreCases`
  (SES-02, WH-02, WH-03, WH-08, RF-02, RF-03, IND-01..03) aborts the run.
  Waived cases are reported as `waived`, do not fail the run, and are listed in
  `Report.Waivers` (verdict `conforms_with_waivers`): audit them in review.
- Not applicable: a case a provider cannot have by design is `not_applicable`
  with the declared reason, never a failure or a waiver. Declared by
  `Target.Supports.NoRefBeforePay` (no provider reference before the buyer
  pays: RF-05) or by the typed error `unsupported_currency`
  (`conformance.UnsupportedCurrencyCode`) when the session asks for JPY / KWD
  (MON-03, MON-06).
- `WebhookMode: WebhookHintReread` is for providers that do not verify a
  signature but answer from their own records (MercadoPago style): WH-02,
  WH-03 and WH-05 accept an altered / unsigned / stale notification only when
  the answer is exactly the genuine payment's; the suite alters the body itself.
  WH-08 forges an unsigned `paid` notification for a still-pending session: it
  must be rejected or answered with the pending state, so a provider that
  believes the body instead of re-reading cannot pass.

## Cases (35)

| Id | What it proves |
|---|---|
| SES-01..05 | contract-shaped session, never `paid` at creation; same `idempotency_key` => same session and exactly one remote session; key reuse with another amount: typed idempotency conflict or the identical session with its amount intact; distinct orders; sandbox (`test_mode=true`) |
| MON-01..09 | more decimals than the currency rejected (10.005 MXN, 10.50 JPY, 10.0005 KWD); 0, negative, `1e3`, `1,000.00`, `.5`, empty rejected; JPY whole units; the provider reports what was really charged (19.50 on a 19.99 session) and the ledger flags `amount_mismatch`; refunds sum exactly (0.10 + 0.20); KWD 10.005 accepted; amounts beyond float64/int64 exact or rejected; invalid / lower-case currency rejected; refund in another currency rejected |
| WH-01..08 | valid signed webhook applied by the ledger rule; the genuine body with a corrupted signature and a body with altered amount / status are rejected (by signature, not by a failed lookup); unsigned (query kept) rejected; replay: same dedupe id, ignored; stale timestamp rejected; late `pending` after `paid` ignored and the payment stays `paid`; second captured payment of the same `order_ref` refused or flagged in `also_captured`; an unsigned forged `paid` for a pending session is not believed |
| ST-01..03 | status agrees with what was charged by `provider_ref` and `order_ref`; unknown reference is an error; `failed` / `expired` final and stable |
| RF-01..07 | partial refund; idempotent on its key and exactly one remote refund; over-refund rejected; full refund => `refunded`; uncaptured payment not refundable; pending refunds count against the remainder; concurrent refunds never exceed the capture |
| IND-01..03 | lost session / refund response is an indeterminate error and the retry leaves exactly one remote session / refund; outage on status is an error (never `paid`, terminal or `not_found`) and the payment reads `paid` again after recovery |

Asynchronous refunds (`pending`) are polled until the payment reflects them; a
refund that never settles is a skip (a failure in strict mode), never a pass.
Rule behind IND: an indeterminate outcome stays `pending`; it is resolved by
retrying with the same key or re-reading the provider, never assumed.

## Deferred to CONF-PAYMENTS@2

`Effects` is counted by the author's own sandbox: it should come from the
provider's remote API; webhooks with a future timestamp; reordered
`paid` -> `refunded` notifications; `test_mode` absent on refund outputs;
multi-account tenancy, 3DS / `requires_action`, disputes, real session expiry,
`commerce.payments.cancel`, `refund.status`, terminals, mixed payments and
`already_paid` (settlement rules of the host, `payments.Settle`). New cases are
additive; changing the behavior of an existing case bumps the suite version.

## CLI

There is no `metacore` CLI in this repository (`cmd/` is empty), so there is no
`metacore conformance` subcommand. When the CLI lands it wraps `conformance.Run`
and `Report.JSON`.
