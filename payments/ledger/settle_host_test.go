package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/capability"
)

// payments.settle is answered by the host without a ledger nor an org: the
// same payments.Settle every channel (POS, cash desk, kiosk) calls.
func TestHostSettle(t *testing.T) {
	h := HostQuerier{}
	ask := func(in map[string]any) (map[string]any, string) {
		t.Helper()
		res, err := h.Query(context.Background(), capability.Query{Capability: SettleKey, Input: in})
		if err != nil {
			t.Fatalf("query %v: %v", in, err)
		}
		if len(res.Results) != 1 || res.Results[0].Provider != HostProvider {
			t.Fatalf("results %+v", res.Results)
		}
		if e := res.Results[0].Err; e != nil {
			var coded *capability.CodedError
			if !errors.As(e, &coded) {
				t.Fatalf("uncoded error %v", e)
			}
			return nil, coded.Code
		}
		var out map[string]any
		if err := json.Unmarshal(res.Results[0].Output, &out); err != nil {
			t.Fatal(err)
		}
		c, _ := capability.Lookup(SettleKey)
		if problems := c.ValidateOutput(res.Results[0].Output); len(problems) > 0 {
			t.Fatalf("output off contract: %v", problems)
		}
		return out, ""
	}
	cases := []struct {
		name                       string
		in                         map[string]any
		code                       string
		applied, change, round, rm string
		settled                    bool
	}{
		{name: "terminal card plus cash, change from cash only", in: map[string]any{"currency": "MXN", "total": "1000.00", "non_cash": "800", "cash": "500"},
			applied: "1000.00", change: "300.00", round: "0.00", rm: "0.00", settled: true},
		{name: "card over the due", in: map[string]any{"currency": "MXN", "total": "1000.00", "non_cash": "1100", "cash": "50"}, code: CodeSettleExceedsTotal},
		{name: "cash rounding", in: map[string]any{"currency": "MXN", "total": "100.49", "cash": "100", "cash_tolerance": "0.50"},
			applied: "100.00", change: "0.00", round: "0.49", rm: "0.00", settled: true},
		{name: "a coin does not settle a small ticket", in: map[string]any{"currency": "MXN", "total": "0.50", "cash": "0.01", "cash_tolerance": "0.99"},
			applied: "0.01", change: "0.00", round: "0.00", rm: "0.49"},
		{name: "partial", in: map[string]any{"currency": "MXN", "total": "300", "already_paid": "100", "non_cash": "50"},
			applied: "50.00", change: "0.00", round: "0.00", rm: "150.00"},
		{name: "nothing due", in: map[string]any{"currency": "MXN", "total": "300", "already_paid": "300", "cash": "1"}, code: CodeSettleNothingDue},
		{name: "tolerance above the cap", in: map[string]any{"currency": "MXN", "total": "100", "cash": "99", "cash_tolerance": "5"}, code: CodeSettleInvalidTolerance},
		{name: "negative", in: map[string]any{"currency": "MXN", "total": "100", "cash": "-1", "non_cash": "101"}, code: CodeSettleNegativeAmount},
		{name: "sub-cent", in: map[string]any{"currency": "MXN", "total": "100", "non_cash": "99.995"}, code: CodeSettleNotQuantized},
		{name: "nothing collected", in: map[string]any{"currency": "MXN", "total": "100"}, code: CodeSettleTenderMismatch},
	}
	for _, c := range cases {
		out, code := ask(c.in)
		if code != c.code {
			t.Errorf("%s: code %q, want %q", c.name, code, c.code)
			continue
		}
		if c.code != "" {
			continue
		}
		if out["applied"] != c.applied || out["change"] != c.change || out["rounding"] != c.round || out["remaining"] != c.rm || out["settled"] != c.settled {
			t.Errorf("%s: %v", c.name, out)
		}
	}
	// The contract input is checked before the rule runs.
	if _, err := h.Query(context.Background(), capability.Query{Capability: SettleKey, Input: map[string]any{"currency": "MXN"}}); err == nil {
		t.Fatal("missing total accepted")
	}
	// Other payments.* contracts still need the ledger.
	if _, err := h.Query(context.Background(), capability.Query{Capability: DocumentGet, Input: map[string]any{"document_type": "x", "document_id": "y"}}); err == nil {
		t.Fatal("ledger contract answered without a ledger")
	}
}
