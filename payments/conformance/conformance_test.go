package conformance_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/payments/conformance"
	"github.com/asteby/metacore-kernel/payments/conformance/refsim"
)

// The reference simulator passes every case: this is also how an addon wires
// its own provider.
func TestReferenceProviderConforms(t *testing.T) {
	rep := conformance.RunT(t, refsim.New().Target("refsim"), conformance.Options{Strict: true})
	if !rep.Conforms() {
		t.Fatalf("reference provider does not conform:\n%s", rep)
	}
	if p, _, _ := rep.Counts(); p != len(conformance.Cases()) {
		t.Fatalf("passed %d of %d cases", p, len(conformance.Cases()))
	}
}

// Each defect must be caught by the case(s) that guard it.
func TestBrokenProvidersFail(t *testing.T) {
	cases := []struct {
		quirk refsim.Quirk
		fails []string
	}{
		{refsim.QuirkNoSignatureCheck, []string{"WH-02", "WH-03"}},
		{refsim.QuirkNoSessionIdempotency, []string{"SES-02"}},
		{refsim.QuirkAcceptSubCents, []string{"MON-01"}},
		{refsim.QuirkAcceptZero, []string{"MON-02"}},
		{refsim.QuirkAssumeFailedOnLost, []string{"IND-01"}},
		{refsim.QuirkNotFoundOnOutage, []string{"IND-03"}},
		{refsim.QuirkPaidOnOutage, []string{"IND-03"}},
		{refsim.QuirkNoRefundIdempotency, []string{"RF-02", "IND-02"}},
		{refsim.QuirkNoOverRefundCheck, []string{"RF-03"}},
		{refsim.QuirkFloatRefundTotals, []string{"MON-05"}},
		{refsim.QuirkFreshEventIDOnReplay, []string{"WH-04"}},
		{refsim.QuirkLiveMode, []string{"SES-05"}},
		{refsim.QuirkRefundUncaptured, []string{"RF-05"}},
	}
	for _, c := range cases {
		t.Run(string(c.quirk), func(t *testing.T) {
			rep := conformance.Run(context.Background(), refsim.New(c.quirk).Target("broken"), conformance.Options{Strict: true})
			if rep.Conforms() {
				t.Fatalf("provider with %s conforms:\n%s", c.quirk, rep)
			}
			failed := map[string]bool{}
			for _, r := range rep.Cases {
				if r.Result == conformance.Fail {
					failed[r.ID] = true
				}
			}
			for _, id := range c.fails {
				if !failed[id] {
					t.Errorf("%s did not fail %s\n%s", c.quirk, id, rep)
				}
			}
		})
	}
}

type noFaults struct{ conformance.Sandbox }

func (noFaults) Inject(context.Context, conformance.Fault) (func(), error) {
	return nil, conformance.ErrFaultUnsupported
}

func TestUnsupportedFaultsSkipUnlessStrict(t *testing.T) {
	tg := refsim.New().Target("nofaults")
	tg.Sandbox = noFaults{tg.Sandbox}
	rep := conformance.Run(context.Background(), tg, conformance.Options{})
	if !rep.Conforms() {
		t.Fatalf("lenient run must conform:\n%s", rep)
	}
	if _, _, s := rep.Counts(); s != 3 {
		t.Fatalf("want the 3 IND cases skipped, got %d\n%s", s, rep)
	}
	if conformance.Run(context.Background(), tg, conformance.Options{Strict: true}).Conforms() {
		t.Fatal("strict run must not conform when indeterminate cases cannot run")
	}
}

func TestWaiveOnlyAndReportJSON(t *testing.T) {
	rep := conformance.Run(context.Background(), refsim.New().Target("x"), conformance.Options{
		Only:  []string{"RF", "WH"},
		Waive: map[string]string{"RF-05": "provider has no pre-payment reference"},
	})
	for _, c := range rep.Cases {
		if !strings.HasPrefix(c.ID, "RF") && !strings.HasPrefix(c.ID, "WH") {
			t.Fatalf("Only leaked %s", c.ID)
		}
		if c.ID == "RF-05" && (c.Result != conformance.Skip || !strings.Contains(c.Detail, "waived")) {
			t.Fatalf("RF-05 not waived: %+v", c)
		}
	}
	raw, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back conformance.Report
	if err := json.Unmarshal(raw, &back); err != nil || back.Suite != "CONF-PAYMENTS@1" || len(back.Cases) != len(rep.Cases) {
		t.Fatalf("report JSON round trip: %v %+v", err, back.Suite)
	}
}

func TestPanickingProviderIsAFailedCase(t *testing.T) {
	tg := refsim.New().Target("panic")
	tg.Invoke = func(context.Context, string, map[string]any) (json.RawMessage, error) { panic("boom") }
	rep := conformance.Run(context.Background(), tg, conformance.Options{Only: []string{"SES-01"}})
	if rep.Conforms() || rep.Cases[0].Result != conformance.Fail {
		t.Fatalf("%s", rep)
	}
}
