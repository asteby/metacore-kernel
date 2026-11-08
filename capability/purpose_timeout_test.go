package capability

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The rates quote is asked at two very different moments: at checkout (a buyer
// waits, so the deadline is short) and after payment to buy a label (nobody
// waits, so a slow carrier gets time). The deadline follows the purpose of the
// call, not the addon that makes it.
func TestShippingRatesDeadlineFollowsThePurposeOfTheCall(t *testing.T) {
	rates := mustContract(t, CommerceShippingRates)
	e := Executor{}
	var none Provider

	cases := []struct {
		name  string
		input map[string]any
		want  time.Duration
	}{
		{"no purpose keeps the checkout deadline", map[string]any{}, ShippingRatesTimeout},
		{"checkout", map[string]any{"purpose": ShippingPurposeCheckout}, ShippingRatesTimeout},
		{"label gets the long deadline", map[string]any{"purpose": ShippingPurposeLabel}, ShippingLabelQuoteTimeout},
		{"purpose is case and space insensitive", map[string]any{"purpose": "  Label "}, ShippingLabelQuoteTimeout},
		{"an unknown purpose falls back to checkout", map[string]any{"purpose": "whatever"}, ShippingRatesTimeout},
		{"a non-string purpose is ignored", map[string]any{"purpose": 42}, ShippingRatesTimeout},
		{"nil input", nil, ShippingRatesTimeout},
	}
	for _, c := range cases {
		if got := e.timeoutFor(rates, none, c.input); got != c.want {
			t.Errorf("%s: deadline = %v, want %v", c.name, got, c.want)
		}
	}

	if ShippingLabelQuoteTimeout <= ShippingRatesTimeout {
		t.Errorf("the label deadline (%v) must be longer than the checkout one (%v)", ShippingLabelQuoteTimeout, ShippingRatesTimeout)
	}
	// A provider's own deadline still wins over the purpose.
	p := Provider{Addon: "carrier", Timeout: 2 * time.Second}
	if got := e.timeoutFor(rates, p, map[string]any{"purpose": ShippingPurposeLabel}); got != 2*time.Second {
		t.Errorf("provider override lost: %v", got)
	}
	// A contract without PurposeTimeouts ignores `purpose` completely.
	plain := Contract{Key: "x.y", Kind: KindQuery, Timeout: 3 * time.Second}
	if got := e.timeoutFor(plain, none, map[string]any{"purpose": ShippingPurposeLabel}); got != 3*time.Second {
		t.Errorf("a contract with no purposes changed its deadline: %v", got)
	}
}

// End to end through Run: a carrier that answers in 120 ms is cut by a 60 ms
// checkout-style deadline but answers when the call says it is for a label.
func TestShippingRatesSlowCarrierAnswersWhenQuotedForALabel(t *testing.T) {
	c := mustContract(t, CommerceShippingRates)
	c.Timeout = 60 * time.Millisecond
	c.PurposeTimeouts = map[string]time.Duration{ShippingPurposeLabel: 2 * time.Second}
	slow := Provider{Addon: "slowpost"}
	invoke := func(ctx context.Context, call Call) (json.RawMessage, error) {
		select {
		case <-time.After(120 * time.Millisecond):
			return json.RawMessage(`{"rates":[]}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	org := uuid.New()

	checkout, err := Executor{SkipOutputValidation: true}.Run(context.Background(), c, org, "mx", []Provider{slow}, ratesInput(), invoke)
	if err != nil {
		t.Fatal(err)
	}
	if checkout.Results[0].OK() || checkout.Results[0].ErrorCode() != "timeout" {
		t.Errorf("checkout quote: want timeout, got ok=%v code=%q", checkout.Results[0].OK(), checkout.Results[0].ErrorCode())
	}

	in := ratesInput()
	in["purpose"] = ShippingPurposeLabel
	label, err := Executor{SkipOutputValidation: true}.Run(context.Background(), c, org, "mx", []Provider{slow}, in, invoke)
	if err != nil {
		t.Fatal(err)
	}
	if !label.Results[0].OK() {
		t.Errorf("label quote must wait for the slow carrier: code=%q err=%v", label.Results[0].ErrorCode(), label.Results[0].Err)
	}
}

// `purpose` is part of the typed input, with exactly the two known values.
func TestShippingRatesInputDeclaresThePurpose(t *testing.T) {
	rates := mustContract(t, CommerceShippingRates)
	if !rates.Has("purpose") {
		t.Fatal("commerce.shipping.rates must declare `purpose`")
	}
	in := ratesInput()
	for _, ok := range []string{ShippingPurposeCheckout, ShippingPurposeLabel} {
		in["purpose"] = ok
		if probs := rates.CheckInput(in); len(probs) > 0 {
			t.Errorf("purpose %q rejected: %v", ok, probs)
		}
	}
	in["purpose"] = "wholesale"
	if probs := rates.CheckInput(in); len(probs) == 0 {
		t.Error("an unknown purpose must be rejected by the typed input")
	}
}
