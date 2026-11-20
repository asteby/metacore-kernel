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
//	     on idempotency_key (counted on the provider side), never "paid" at creation
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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// ErrUnsupported is returned by a Sandbox method it cannot implement (a fault
// it cannot inject, effects it cannot count, aging a webhook). The dependent
// cases are skipped, and a skip fails the run unless Options.Lenient.
var ErrUnsupported = errors.New("conformance: not supported by this sandbox")

// SettleSpec says how the buyer finishes a session.
type SettleSpec struct {
	SessionID string
	// Status is paid, failed, expired or cancelled; pending asks for a LATE
	// notification that says the payment is still pending (sent after paid).
	Status payments.Status
	// Amount and Currency are what was REALLY charged; empty means the
	// session's. A provider must report this, not echo the session.
	Amount   string
	Currency string
}

// Effects are the remote effects the sandbox counts for one order_ref: the
// ground truth that a retry did not create a second session or refund.
type Effects struct {
	Sessions int
	Refunds  int
}

// Sandbox drives the provider's sandbox / simulator from the side of the
// "outside world" (the buyer, the provider's notification system, the
// network). It must only ever talk to a sandbox: the run aborts (preflight,
// not waivable) when the provider does not report test_mode=true.
type Sandbox interface {
	// Settle makes the buyer finish the session and returns the SIGNED
	// notification the provider would send. The status is also what the
	// provider API reports from then on (a late pending never regresses it).
	Settle(ctx context.Context, spec SettleSpec) (Webhook, error)
	// Age returns w re-issued as if it had been sent `by` ago (valid
	// signature, old timestamp). ErrUnsupported when the provider has no
	// replay window to test.
	Age(ctx context.Context, w Webhook, by time.Duration) (Webhook, error)
	// Effects counts, on the provider side, the sessions and refunds that
	// really exist for orderRef. ErrUnsupported when it cannot.
	Effects(ctx context.Context, orderRef string) (Effects, error)
	// Inject arms fault for the next matching call and returns the function
	// that disarms it. ErrUnsupported when not available.
	Inject(ctx context.Context, f Fault) (restore func(), err error)
}

// WebhookMode is how the provider authenticates notifications.
type WebhookMode string

const (
	// WebhookSigned (default): the notification carries a signature; altered,
	// unsigned and stale ones must be rejected.
	WebhookSigned WebhookMode = "signed"
	// WebhookHintReread: the provider does not verify a signature but treats
	// the notification as a hint and re-reads the payment from its API
	// (MercadoPago style). An altered or unsigned notification may be
	// accepted only if the answer is exactly the genuine payment's.
	WebhookHintReread WebhookMode = "hint_reread"
)

// Target is the provider under test.
type Target struct {
	// Name is reported only (e.g. "mercadopago").
	Name    string
	Invoke  Invoker
	Sandbox Sandbox
	// Supports is what the provider declares it cannot do by design. A case
	// that depends on it is reported not_applicable (with the declared reason)
	// instead of failing a strict run.
	Supports Supports
}

// Supports are the provider's declared limits.
type Supports struct {
	// NoRefBeforePay: the provider holds no payment reference until the buyer
	// pays, so a pending payment cannot even be named for a refund (RF-05).
	// The text is the reason shown in the report.
	NoRefBeforePay string
	// UnsupportedCurrencies are currencies the provider cannot charge at all
	// (it answers the typed unsupported_currency error). Without the
	// declaration, refusing a currency is a failure; with it, the refusal must
	// be consistent (every well-quantized amount of that currency), or the case
	// fails.
	UnsupportedCurrencies []string
}

// UnsupportedCurrencyCode is the typed error (capability.NewCodedError) a
// provider answers when it cannot charge a currency at all. MON-03 (JPY) and
// MON-06 (KWD) are then not_applicable for it, if it declared the currency.
const UnsupportedCurrencyCode = "unsupported_currency"

// Options tune a run. The zero value is the strict, complete run.
type Options struct {
	// Only restricts the run to case ids with one of these prefixes
	// ("SES", "WH-02"). A restricted run is partial: Conforms() is false and
	// the report says so.
	Only []string
	// Waive exempts case ids (they must exist) for a written, non-blank
	// reason. Waived cases are reported as "waived" with the reason, and the
	// list travels in Report.Waivers; they do not fail the run. At most
	// MaxWaivers may be given, and the CoreCases cannot be waived at all:
	// asking for it aborts the run. The preflight (sandbox only) and the
	// per-call test_mode / x_environment checks cannot be waived either.
	Waive map[string]string
	// Lenient tolerates skipped cases (a sandbox that cannot inject a fault,
	// count effects or age a webhook; an asynchronous refund that never
	// settled). The verdict is then "conforms_lenient", never "conforms", and
	// Conforms() is false. The default is strict: a skip fails the run.
	Lenient bool
	// WebhookMode defaults to WebhookSigned.
	WebhookMode WebhookMode
	// Timeout bounds each provider call (default 10s).
	Timeout time.Duration
	// PollTimeout bounds the wait for an asynchronous state (a pending refund)
	// to settle (default 3s).
	PollTimeout time.Duration
	// RunID makes order refs and idempotency keys unique per run (default:
	// random).
	RunID string
}

// MaxWaivers is the most cases a run may waive.
const MaxWaivers = 5

// CoreCases are the cases that prove the money cannot be lost, duplicated or
// forged; they can never be waived. A provider that truly cannot run them
// does not conform (Lenient reports it as conforms_lenient).
var CoreCases = []string{"SES-02", "WH-02", "WH-03", "WH-08", "RF-02", "RF-03", "IND-01", "IND-02", "IND-03"}

// Result states.
const (
	Pass = "pass"
	Fail = "fail"
	// Skip: the case could not conclude (unsupported sandbox capability,
	// refund never settled). Fails a strict run.
	Skip = "skip"
	// Waived: exempted by Options.Waive, with the reason in the report.
	Waived = "waived"
	// NotApplicable: the provider declared (Target.Supports, or the typed
	// unsupported_currency error) that the case cannot apply to it, with the
	// reason. It never fails a run.
	NotApplicable = "not_applicable"
)

// CaseResult is the outcome of one case.
type CaseResult struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// Waiver is an exemption applied to a run.
type Waiver struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Report is the result of a run; it marshals to the JSON a CI can archive.
type Report struct {
	Suite       string      `json:"suite"`
	Provider    string      `json:"provider"`
	RunID       string      `json:"run_id"`
	WebhookMode WebhookMode `json:"webhook_mode"`
	Lenient     bool        `json:"lenient,omitempty"`
	// Only is the case filter of a partial run (empty = the whole suite).
	Only    []string `json:"only,omitempty"`
	Waivers []Waiver `json:"waivers,omitempty"`
	// Limits are the not_applicable cases with the provider's declared reason.
	Limits []Waiver `json:"limits,omitempty"`
	// Aborted is why no case ran (invalid options, not a sandbox, ...).
	Aborted string       `json:"aborted,omitempty"`
	Cases   []CaseResult `json:"cases"`
}

// Tally counts the cases of a report by state.
type Tally struct{ Pass, Fail, Skip, Waived, NotApplicable int }

// Counts tallies the cases by state.
func (r Report) Counts() Tally {
	var t Tally
	for _, c := range r.Cases {
		switch c.Result {
		case Pass:
			t.Pass++
		case Fail:
			t.Fail++
		case Waived:
			t.Waived++
		case NotApplicable:
			t.NotApplicable++
		default:
			t.Skip++
		}
	}
	return t
}

// Partial reports whether the run was restricted with Options.Only.
func (r Report) Partial() bool { return len(r.Only) > 0 }

// Verdict: "aborted", "partial" (Only), "fails", "conforms_lenient" (no
// failure but some case was skipped and Lenient tolerated it),
// "conforms_with_waivers", "conforms_with_limits" (some cases not_applicable
// by the provider's declaration, listed in Limits) or "conforms".
func (r Report) Verdict() string {
	t := r.Counts()
	switch {
	case r.Aborted != "":
		return "aborted"
	case r.Partial():
		return "partial"
	case t.Fail > 0 || t.Pass == 0 || (t.Skip > 0 && !r.Lenient):
		return "fails"
	case t.Skip > 0:
		return "conforms_lenient"
	case len(r.Waivers) > 0:
		return "conforms_with_waivers"
	case t.NotApplicable > 0:
		return "conforms_with_limits"
	}
	return "conforms"
}

// Conforms reports whether the provider conforms to CONF-PAYMENTS@1: the
// verdict is conforms, conforms_with_limits or conforms_with_waivers (a complete run, nothing
// failed, nothing skipped; waivers are listed in Waivers: trace them).
// conforms_lenient is NOT conformance: some case could not run.
func (r Report) Conforms() bool {
	v := r.Verdict()
	return v == "conforms" || v == "conforms_with_waivers" || v == "conforms_with_limits"
}

// String is a human-readable table.
func (r Report) String() string {
	var b strings.Builder
	t := r.Counts()
	fmt.Fprintf(&b, "%s provider=%s run=%s mode=%s: %s (%d passed, %d failed, %d skipped, %d waived, %d not applicable)\n",
		r.Suite, r.Provider, r.RunID, r.WebhookMode, r.Verdict(), t.Pass, t.Fail, t.Skip, t.Waived, t.NotApplicable)
	if r.Aborted != "" {
		fmt.Fprintf(&b, "  ABORTED %s\n", r.Aborted)
	}
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "  %-6s %-8s %s", strings.ToUpper(c.Result), c.ID, c.Title)
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
// a misbehaving provider: a panic in a call is a failed case. Before any case
// it runs the preflight (the provider must be a sandbox); neither Only nor
// Waive can skip it.
func Run(ctx context.Context, t Target, opts Options) Report {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = 3 * time.Second
	}
	if opts.WebhookMode == "" {
		opts.WebhookMode = WebhookSigned
	}
	if opts.RunID == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		opts.RunID = hex.EncodeToString(b[:])
	}
	rep := Report{Suite: Suite, Provider: t.Name, RunID: opts.RunID, WebhookMode: opts.WebhookMode,
		Lenient: opts.Lenient, Only: opts.Only}
	abort := func(format string, a ...any) Report { rep.Aborted = fmt.Sprintf(format, a...); return rep }
	if t.Invoke == nil || t.Sandbox == nil {
		return abort("target needs Invoke and Sandbox")
	}
	if opts.WebhookMode != WebhookSigned && opts.WebhookMode != WebhookHintReread {
		return abort("unknown WebhookMode %q", opts.WebhookMode)
	}
	known := map[string]bool{}
	for _, c := range allCases {
		known[c.ID] = true
	}
	if len(opts.Waive) > MaxWaivers {
		return abort("%d waivers: at most %d may be waived", len(opts.Waive), MaxWaivers)
	}
	for id, reason := range opts.Waive {
		for _, core := range CoreCases {
			if id == core {
				return abort("%s is a core case and cannot be waived", id)
			}
		}
		if !known[id] {
			return abort("waiver for unknown case %q", id)
		}
		if strings.TrimSpace(reason) == "" {
			return abort("waiver of %s has no reason", id)
		}
		rep.Waivers = append(rep.Waivers, Waiver{id, reason})
	}
	sort.Slice(rep.Waivers, func(i, j int) bool { return rep.Waivers[i].ID < rep.Waivers[j].ID })
	if len(opts.Only) > 0 {
		any := false
		for _, c := range allCases {
			any = any || selected(c.ID, opts.Only)
		}
		if !any {
			return abort("Only %v matches no case", opts.Only)
		}
	}
	if err := preflight(ctx, t, opts); err != nil {
		return abort("preflight: %v", err)
	}
	for _, c := range allCases {
		if !selected(c.ID, opts.Only) {
			continue
		}
		if reason, ok := opts.Waive[c.ID]; ok {
			rep.Cases = append(rep.Cases, CaseResult{c.ID, c.Title, Waived, reason})
			continue
		}
		rep.Cases = append(rep.Cases, runCase(ctx, t, opts, c))
	}
	for _, c := range rep.Cases {
		if c.Result == NotApplicable {
			rep.Limits = append(rep.Limits, Waiver{c.ID, c.Detail})
		}
	}
	return rep
}

// preflight refuses to run against anything that does not say it is a sandbox.
func preflight(ctx context.Context, t Target, opts Options) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	e := &env{ctx: ctx, t: t, opts: opts, id: "pre"}
	if _, err := e.session(e.newOrder("1.00", "MXN")); err != nil {
		return fmt.Errorf("the provider must answer a session with test_mode=true: %w", err)
	}
	return nil
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
	case errors.Is(err, errNA):
		res.Result, res.Detail = NotApplicable, strings.TrimPrefix(err.Error(), errNA.Error()+": ")
	case errors.Is(err, errSkip):
		res.Result, res.Detail = Skip, err.Error()
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
//	        conformance.Options{})
//	}
//
// RunT fails the test when the report does not conform (including partial,
// aborted and, unless Lenient, skipped).
func RunT(t *testing.T, target Target, opts Options) Report {
	t.Helper()
	return RunTB(t, target, opts)
}

// Reporter is the part of *testing.T that RunTB uses (tests may fake it).
type Reporter interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Run(name string, f func(*testing.T)) bool
}

// RunTB is RunT over a Reporter.
func RunTB(t Reporter, target Target, opts Options) Report {
	t.Helper()
	rep := Run(context.Background(), target, opts)
	if rep.Aborted != "" {
		t.Fatalf("CONF-PAYMENTS aborted, nothing was certified: %s", rep.Aborted)
	}
	for _, c := range rep.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			switch c.Result {
			case Fail:
				t.Errorf("%s: %s", c.Title, c.Detail)
			case Skip:
				if rep.Lenient {
					t.Skipf("%s: %s", c.Title, c.Detail)
				}
				t.Errorf("skipped (strict): %s: %s", c.Title, c.Detail)
			case Waived:
				t.Skipf("waived: %s", c.Detail)
			case NotApplicable:
				t.Skipf("not applicable: %s", c.Detail)
			}
		})
	}
	if rep.Partial() {
		t.Errorf("PARTIAL run (Options.Only %v): not a conformance result, the test fails on purpose; filter with `go test -run` instead", rep.Only)
	}
	switch rep.Verdict() {
	case "conforms_lenient":
		t.Logf("NOT CERTIFIED: Lenient tolerated %d skipped case(s); verdict conforms_lenient is not conformance", rep.Counts().Skip)
	case "fails":
		if rep.Counts().Fail == 0 && !rep.Lenient {
			t.Errorf("verdict fails: %d skipped / no case passed", rep.Counts().Skip)
		}
	}
	t.Logf("\n%s", rep.String())
	return rep
}
