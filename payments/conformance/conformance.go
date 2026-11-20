// Package conformance is CONF-PAYMENTS@1: the reusable conformance suite a
// payments provider (mercadopago, clip, stripe, a terminal ...) runs against
// its own sandbox or simulator to prove it honours the commerce.payments.*
// contracts of package capability and the rules of package payments.
//
// The suite is provider-neutral and speaks the capability wire form: it
// invokes the provider through an Invoker (capability key + contract input in,
// raw JSON output out; the same shape a host uses) and drives the sandbox
// through the small Sandbox interface (settle a session, sign / forge a
// webhook, inject faults). No manifest schema is involved: it only uses the
// contracts that exist today (commerce.payments.session / confirm / status /
// refund).
//
// What it checks (case ids are stable; see Suite):
//
//	SES  session: contract-shaped output, sandbox only (test_mode), idempotent
//	     on idempotency_key, never "paid" at creation
//	MON  money: decimal strings, quantized to the currency (centavos), zero
//	     decimal currencies, exact sums (0.10 + 0.20 = 0.30)
//	WH   webhook: valid signature accepted, tampered / unsigned rejected,
//	     replay yields the same dedupe id and is ignored by the ledger rule
//	ST   status: provider is the source of truth, final states, not_found only
//	     when authoritative
//	RF   refund: idempotent on its key, no over-refund, exact totals
//	IND  indeterminate: a timeout is an error the caller may only resolve by
//	     retrying with the same key / re-reading; the provider never turns an
//	     unknown outcome into paid / failed / not_found
//
// An addon runs it from its CI as an ordinary Go test (see RunT and
// docs/conformance-payments.md). There is no `metacore` CLI in this repository
// yet, so there is no `metacore conformance` subcommand: when the CLI lands it
// is a thin wrapper over Run and Report.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
)

// Suite is the identifier and version of the suite.
const Suite = "CONF-PAYMENTS@1"

// Invoker calls one commerce.payments.* capability of the provider under
// test: input is the contract input, the result the raw JSON output. A
// provider that fails returns an error; the kinds the suite distinguishes are
// capability.ErrTimeout / capability.ErrIndeterminate / context.DeadlineExceeded
// (outcome unknown), *capability.CodedError (typed provider error such as
// not_found) and anything else (provider_error).
type Invoker func(ctx context.Context, capabilityKey string, input map[string]any) (json.RawMessage, error)

// FromCapability adapts a host-side capability.Invoker (the one the Executor
// uses) to an Invoker. The idempotency key is lifted from the input as the
// host does.
func FromCapability(inv capability.Invoker, provider capability.Provider) Invoker {
	return func(ctx context.Context, key string, input map[string]any) (json.RawMessage, error) {
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		idem, _ := input["idempotency_key"].(string)
		return inv(ctx, capability.Call{Capability: key, Provider: provider, IdempotencyKey: idem, Payload: raw})
	}
}

// Webhook is a provider notification as the host hands it to
// commerce.payments.confirm.
type Webhook struct {
	// RawBody is the exact body bytes (the signature covers them).
	RawBody string
	// Headers has lowercase keys (signature, timestamp ...).
	Headers map[string]string
	// Query is the query string of the notification URL.
	Query map[string]string
}

// Fault is a failure the sandbox can inject on the NEXT call of the provider.
type Fault string

const (
	// FaultSessionLostResponse: the provider creates the session but the
	// answer never reaches the caller (timeout after the effect).
	FaultSessionLostResponse Fault = "session_lost_response"
	// FaultRefundLostResponse: same, for commerce.payments.refund.
	FaultRefundLostResponse Fault = "refund_lost_response"
	// FaultProviderDown: the provider API is unreachable for reads
	// (commerce.payments.status) until restore is called.
	FaultProviderDown Fault = "provider_down"
)

// ErrFaultUnsupported is returned by Sandbox.Inject when the sandbox cannot
// inject that fault. The dependent cases are reported as skipped, and fail
// under Options.Strict.
var ErrFaultUnsupported = errors.New("conformance: fault not supported by this sandbox")

// Sandbox drives the provider's sandbox / simulator from the side of the
// "outside world" (the buyer, the provider's notification system, the
// network). It must only ever talk to a sandbox: the suite also requires the
// provider to report test_mode=true.
type Sandbox interface {
	// Settle makes the buyer finish session sessionID with status (paid,
	// failed, expired or cancelled) and returns the SIGNED notification the
	// provider would send. The status is also what the provider API reports
	// from then on.
	Settle(ctx context.Context, sessionID string, status payments.Status) (Webhook, error)
	// Forge returns w with its content altered (the amount raised) but the
	// original signature: a provider must reject it.
	Forge(w Webhook) Webhook
	// Inject arms fault for the next matching call and returns the function
	// that disarms it. ErrFaultUnsupported when not available.
	Inject(ctx context.Context, f Fault) (restore func(), err error)
}

// Target is the provider under test.
type Target struct {
	// Name is reported only (e.g. "mercadopago").
	Name    string
	Invoke  Invoker
	Sandbox Sandbox
}

// Options tune a run.
type Options struct {
	// Only restricts the run to case ids with one of these prefixes
	// ("SES", "WH-02").
	Only []string
	// Waive maps a case id to the reason the provider is exempt (a provider
	// without refunds waives RF-*). Waived cases are reported as skipped and
	// never fail Strict; the reason is part of the report.
	Waive map[string]string
	// Strict makes a skipped (unsupported fault) case a failure. Use it in the
	// CI of providers that are expected to be fully testable.
	Strict bool
	// Timeout bounds each provider call (default 10s).
	Timeout time.Duration
	// RunID makes order refs and idempotency keys unique per run (default:
	// the clock). Set it to reproduce a run.
	RunID string
}

// Result states.
const (
	Pass = "pass"
	Fail = "fail"
	Skip = "skip"
)

// CaseResult is the outcome of one case.
type CaseResult struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// Report is the result of a run; it marshals to the JSON a CI can archive.
type Report struct {
	Suite    string       `json:"suite"`
	Provider string       `json:"provider"`
	RunID    string       `json:"run_id"`
	Strict   bool         `json:"strict,omitempty"`
	Cases    []CaseResult `json:"cases"`
}

// Counts returns the number of passed, failed and skipped cases.
func (r Report) Counts() (pass, fail, skip int) {
	for _, c := range r.Cases {
		switch c.Result {
		case Pass:
			pass++
		case Fail:
			fail++
		default:
			skip++
		}
	}
	return
}

// Conforms reports whether no case failed (and, under Strict, none skipped
// for lack of sandbox support) and at least one case passed.
func (r Report) Conforms() bool {
	p, f, s := r.Counts()
	return f == 0 && p > 0 && (!r.Strict || s == 0)
}

// String is a human-readable table.
func (r Report) String() string {
	var b strings.Builder
	p, f, s := r.Counts()
	fmt.Fprintf(&b, "%s provider=%s run=%s: %d passed, %d failed, %d skipped\n", r.Suite, r.Provider, r.RunID, p, f, s)
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "  %-4s %-8s %s", strings.ToUpper(c.Result), c.ID, c.Title)
		if c.Detail != "" {
			fmt.Fprintf(&b, " -- %s", c.Detail)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// JSON marshals the report.
func (r Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Case is one conformance case.
type Case struct {
	ID    string
	Title string
	run   func(*env) error
}

// Cases lists the cases of the suite in run order.
func Cases() []Case {
	out := make([]Case, len(allCases))
	copy(out, allCases)
	return out
}

// Run executes the suite against t and returns the report. It never panics on
// a misbehaving provider: a panic in a call is a failed case.
func Run(ctx context.Context, t Target, opts Options) Report {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.RunID == "" {
		opts.RunID = fmt.Sprintf("%x", time.Now().UnixNano()/1e6)
	}
	rep := Report{Suite: Suite, Provider: t.Name, RunID: opts.RunID, Strict: opts.Strict}
	for _, c := range allCases {
		if !selected(c.ID, opts.Only) {
			continue
		}
		if reason, ok := opts.Waive[c.ID]; ok {
			rep.Cases = append(rep.Cases, CaseResult{c.ID, c.Title, Skip, "waived: " + reason})
			continue
		}
		rep.Cases = append(rep.Cases, runCase(ctx, t, opts, c))
	}
	return rep
}

func selected(id string, only []string) bool {
	if len(only) == 0 {
		return true
	}
	for _, p := range only {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

func runCase(ctx context.Context, t Target, opts Options, c Case) (res CaseResult) {
	res = CaseResult{ID: c.ID, Title: c.Title}
	defer func() {
		if r := recover(); r != nil {
			res.Result, res.Detail = Fail, fmt.Sprintf("panic: %v", r)
		}
	}()
	e := &env{ctx: ctx, t: t, opts: opts, id: c.ID}
	defer e.cleanup()
	err := c.run(e)
	switch {
	case err == nil:
		res.Result = Pass
	case errors.Is(err, errSkip):
		res.Result, res.Detail = Skip, err.Error()
		if opts.Strict {
			res.Result = Fail
		}
	default:
		res.Result, res.Detail = Fail, err.Error()
	}
	return res
}

// RunT runs the suite as subtests of tb's *testing.T and fails the test for
// every failed case. It is the entry point for an addon's CI:
//
//	func TestConformancePayments(t *testing.T) {
//	    conformance.RunT(t, conformance.Target{Name: "acme", Invoke: invoke, Sandbox: sb},
//	        conformance.Options{Strict: true})
//	}
func RunT(t *testing.T, target Target, opts Options) Report {
	t.Helper()
	rep := Run(context.Background(), target, opts)
	for _, c := range rep.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			switch c.Result {
			case Fail:
				t.Errorf("%s: %s", c.Title, c.Detail)
			case Skip:
				t.Skipf("%s: %s", c.Title, c.Detail)
			}
		})
	}
	t.Log("\n" + rep.String())
	return rep
}
