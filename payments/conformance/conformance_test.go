package conformance_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/payments/conformance"
	"github.com/asteby/metacore-kernel/payments/conformance/refsim"
)

func run(q ...refsim.Quirk) conformance.Report {
	return conformance.Run(context.Background(), refsim.New(q...).Target("refsim"), conformance.Options{PollTimeout: 300 * time.Millisecond})
}

// The reference simulator passes every case (strict is the default): this is
// also how an addon wires its own provider.
func TestReferenceProviderConforms(t *testing.T) {
	rep := conformance.RunT(t, refsim.New().Target("refsim"), conformance.Options{})
	if rep.Verdict() != "conforms" {
		t.Fatalf("reference provider does not conform:\n%s", rep)
	}
	if p, _, _, _ := rep.Counts(); p != len(conformance.Cases()) {
		t.Fatalf("passed %d of %d cases", p, len(conformance.Cases()))
	}
}

// Asynchronous refunds are legitimate: the suite polls until they settle.
func TestAsyncRefundsConform(t *testing.T) {
	if rep := run(refsim.AsyncRefunds); !rep.Conforms() {
		t.Fatalf("async refunds must conform:\n%s", rep)
	}
}

// Each defect must be caught by the case(s) that guard it.
func TestPendingRefundsMustReserve(t *testing.T) {
	if rep := run(refsim.AsyncRefunds, refsim.QuirkPendingRefundsIgnored); rep.Conforms() || !failed(rep, "RF-06") {
		t.Fatalf("%s", rep)
	}
}

func failed(r conformance.Report, id string) bool {
	for _, c := range r.Cases {
		if c.ID == id && c.Result == conformance.Fail {
			return true
		}
	}
	return false
}

func TestBrokenProvidersFail(t *testing.T) {
	cases := []struct {
		quirk refsim.Quirk
		fails []string
	}{
		{refsim.QuirkNoSignatureCheck, []string{"WH-02", "WH-03"}},
		{refsim.QuirkNoTimestampCheck, []string{"WH-05"}},
		{refsim.QuirkNoSessionIdempotency, []string{"SES-02"}},
		{refsim.QuirkDoubleSessionAfterLost, []string{"IND-01"}},
		{refsim.QuirkKeyReuseMutatesAmount, []string{"SES-03"}},
		{refsim.QuirkAcceptSubCents, []string{"MON-01"}},
		{refsim.QuirkAcceptZero, []string{"MON-02"}},
		{refsim.QuirkTwoDecimalsOnly, []string{"MON-06"}},
		{refsim.QuirkFloatAmounts, []string{"MON-07"}},
		{refsim.QuirkAcceptLowercaseCcy, []string{"MON-08"}},
		{refsim.QuirkRefundCurrencyUnchecked, []string{"MON-09"}},
		{refsim.QuirkEchoSessionAmount, []string{"MON-04", "ST-01"}},
		{refsim.QuirkAssumeFailedOnLost, []string{"IND-01"}},
		{refsim.QuirkNotFoundOnOutage, []string{"IND-03"}},
		{refsim.QuirkPaidOnOutage, []string{"IND-03"}},
		{refsim.QuirkNoRecoveryAfterOutage, []string{"IND-03"}},
		{refsim.QuirkNoRefundIdempotency, []string{"RF-02"}},
		{refsim.QuirkDoubleRefundAfterLost, []string{"IND-02"}},
		{refsim.QuirkNoOverRefundCheck, []string{"RF-03", "RF-07"}},

		{refsim.QuirkFloatRefundTotals, []string{"MON-05"}},
		{refsim.QuirkFreshEventIDOnReplay, []string{"WH-04"}},
		{refsim.QuirkLateWebhookRegresses, []string{"WH-06"}},
		{refsim.QuirkNoAlsoCaptured, []string{"WH-07"}},
		{refsim.QuirkLiveStatus, []string{"WH-01", "ST-01"}},
		{refsim.QuirkRefundUncaptured, []string{"RF-05"}},
		// refunds that answer pending and never settle are inconclusive:
		// skipped, which a strict run does not accept (never a pass)
		{refsim.QuirkRefundPendingForever, []string{"RF-01", "RF-04", "IND-02"}},
	}
	for _, c := range cases {
		t.Run(string(c.quirk), func(t *testing.T) {
			rep := run(c.quirk)
			if rep.Conforms() {
				t.Fatalf("provider with %s conforms:\n%s", c.quirk, rep)
			}
			bad := map[string]bool{}
			for _, r := range rep.Cases {
				if r.Result == conformance.Fail || r.Result == conformance.Skip {
					bad[r.ID] = true
				}
			}
			for _, id := range c.fails {
				if !bad[id] {
					t.Errorf("%s did not fail %s\n%s", c.quirk, id, rep)
				}
			}
		})
	}
}

// A provider that reports production is refused before any case runs, and no
// option can get around it.
func TestPreflightIsNotAvoidable(t *testing.T) {
	tg := refsim.New(refsim.QuirkLiveMode, refsim.QuirkNoSignatureCheck).Target("live")
	for name, o := range map[string]conformance.Options{
		"full":    {},
		"only":    {Only: []string{"SES-01", "MON"}},
		"waived":  {Waive: map[string]string{"SES-05": "not needed"}},
		"lenient": {Lenient: true},
	} {
		rep := conformance.Run(context.Background(), tg, o)
		if rep.Conforms() || rep.Verdict() != "aborted" || len(rep.Cases) != 0 || !strings.Contains(rep.Aborted, "test_mode") {
			t.Errorf("%s: %s", name, rep)
		}
	}
}

func TestPartialRunNeverConforms(t *testing.T) {
	rep := conformance.Run(context.Background(), refsim.New(refsim.QuirkNoSignatureCheck).Target("x"),
		conformance.Options{Only: []string{"SES-01", "MON-04"}})
	if rep.Conforms() || rep.Verdict() != "partial" || len(rep.Only) != 2 {
		t.Fatalf("a filtered run must be partial, never conforming:\n%s", rep)
	}
	ok := conformance.Run(context.Background(), refsim.New().Target("x"), conformance.Options{Only: []string{"SES"}})
	if ok.Conforms() || ok.Verdict() != "partial" {
		t.Fatalf("even a clean filtered run is partial:\n%s", ok)
	}
}

func TestWaiversAreExplicitAndTraceable(t *testing.T) {
	tg := refsim.New().Target("w")
	rep := conformance.Run(context.Background(), tg, conformance.Options{Waive: map[string]string{"RF-05": "no pre-payment reference"}})
	if !rep.Conforms() || rep.Verdict() != "conforms_with_waivers" || len(rep.Waivers) != 1 || rep.Waivers[0].ID != "RF-05" {
		t.Fatalf("waived run:\n%s", rep)
	}
	raw, _ := rep.JSON()
	if !strings.Contains(string(raw), `"waivers"`) || !strings.Contains(string(raw), `"waived"`) {
		t.Fatalf("waivers missing from the JSON: %s", raw)
	}
	for name, w := range map[string]map[string]string{
		"unknown id":   {"RF-99": "x"},
		"blank reason": {"RF-05": "  "},
	} {
		if r := conformance.Run(context.Background(), tg, conformance.Options{Waive: w}); r.Conforms() || r.Verdict() != "aborted" {
			t.Errorf("%s must abort the run:\n%s", name, r)
		}
	}
	if r := conformance.Run(context.Background(), tg, conformance.Options{Only: []string{"ZZ"}}); r.Verdict() != "aborted" {
		t.Errorf("Only matching nothing must abort:\n%s", r)
	}
}

// A waiver cannot hide a broken provider elsewhere.
func TestWaiverDoesNotHideOtherFailures(t *testing.T) {
	rep := conformance.Run(context.Background(), refsim.New(refsim.QuirkNoSignatureCheck).Target("w"),
		conformance.Options{Waive: map[string]string{"RF-05": "n/a"}})
	if rep.Conforms() {
		t.Fatalf("%s", rep)
	}
}

type limited struct{ conformance.Sandbox }

func (limited) Inject(context.Context, conformance.Fault) (func(), error) {
	return nil, conformance.ErrUnsupported
}
func (limited) Effects(context.Context, string) (conformance.Effects, error) {
	return conformance.Effects{}, conformance.ErrUnsupported
}
func (limited) Age(context.Context, conformance.Webhook, time.Duration) (conformance.Webhook, error) {
	return conformance.Webhook{}, conformance.ErrUnsupported
}

func TestUnsupportedSandboxCapabilitiesSkipOnlyWhenLenient(t *testing.T) {
	tg := refsim.New().Target("limited")
	tg.Sandbox = limited{tg.Sandbox}
	strict := conformance.Run(context.Background(), tg, conformance.Options{})
	if strict.Conforms() {
		t.Fatalf("strict must not conform when cases cannot run:\n%s", strict)
	}
	lenient := conformance.Run(context.Background(), tg, conformance.Options{Lenient: true})
	if !lenient.Conforms() {
		t.Fatalf("lenient run must conform:\n%s", lenient)
	}
	var skipped []string
	for _, c := range lenient.Cases {
		if c.Result == conformance.Skip {
			skipped = append(skipped, c.ID)
		}
	}
	sort.Strings(skipped)
	if len(skipped) < 6 {
		t.Fatalf("skipped %v", skipped)
	}
}

// hint+reread providers do not verify signatures but answer from their own
// records: an altered or unsigned notification may be answered only with the
// genuine state. A signature-less provider that cross-checks against its
// records conforms in hint mode and fails in signed mode.
func TestHintRereadMode(t *testing.T) {
	tg := refsim.New(refsim.QuirkNoSignatureCheck, refsim.QuirkNoTimestampCheck).Target("hint")
	if rep := conformance.Run(context.Background(), tg, conformance.Options{WebhookMode: conformance.WebhookHintReread}); !rep.Conforms() {
		t.Fatalf("hint mode:\n%s", rep)
	}
	if rep := conformance.Run(context.Background(), tg, conformance.Options{}); rep.Conforms() || !failed(rep, "WH-03") {
		t.Fatalf("signed mode must reject a signature-less provider:\n%s", rep)
	}
	// a hint provider that believes the notification amount instead of its records fails
	bad := refsim.New(refsim.QuirkNoSignatureCheck, refsim.QuirkNoTimestampCheck, refsim.QuirkEchoSessionAmount).Target("bad")
	if rep := conformance.Run(context.Background(), bad, conformance.Options{WebhookMode: conformance.WebhookHintReread}); rep.Conforms() {
		t.Fatalf("%s", rep)
	}
	if rep := conformance.Run(context.Background(), refsim.New().Target("ref"), conformance.Options{WebhookMode: conformance.WebhookHintReread}); !rep.Conforms() {
		t.Fatalf("%s", rep)
	}
}

func TestReportJSONAndPanic(t *testing.T) {
	rep := run()
	raw, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back conformance.Report
	if err := json.Unmarshal(raw, &back); err != nil || back.Suite != "CONF-PAYMENTS@1" || len(back.Cases) != len(rep.Cases) {
		t.Fatalf("report JSON round trip: %v", err)
	}
	tg := refsim.New().Target("panic")
	tg.Invoke = func(context.Context, string, map[string]any) (json.RawMessage, error) { panic("boom") }
	if r := conformance.Run(context.Background(), tg, conformance.Options{}); r.Conforms() {
		t.Fatalf("%s", r)
	}
}
