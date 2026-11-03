package capability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// commerce.shipping.label declares the refusals a carrier gives BEFORE it
// creates anything (ShippingLabelErrors): they reach the consumer typed, so
// warehouse stops instead of retrying eight times. Everything doubtful (a code
// the contract does not declare, a plain error, a timeout) keeps the generic
// reading it had: provider_error or timeout, so the consumer still retries
// with the same idempotency_key. ProtekAuto SO-00020: Skydropx's
// insufficient_credits arrived as provider_error and was retried 8 times.
func runLabel(t *testing.T, c Contract, invoke Invoker) Result {
	t.Helper()
	res, err := Executor{}.Run(context.Background(), c, org, "mx", []Provider{{Addon: "skydropx"}},
		map[string]any{"rate_id": "r1", "order_ref": "SO-00020", "idempotency_key": "ship-SO-00020"}, invoke)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res.Results[0]
}

func failWith(err error) Invoker {
	return func(context.Context, Call) (json.RawMessage, error) { return nil, err }
}

func TestShippingLabelDeclaredCodesPassThrough(t *testing.T) {
	c := mustContract(t, CommerceShippingLabel)
	if strings.Join(c.Errors, ",") != strings.Join(ShippingLabelErrors, ",") {
		t.Fatalf("label errors = %v, want %v", c.Errors, ShippingLabelErrors)
	}
	for _, code := range ShippingLabelErrors {
		r := runLabel(t, c, failWith(NewCodedError(code, "detalle "+code)))
		if r.ErrorCode() != code {
			t.Fatalf("declared %s: code = %q", code, r.ErrorCode())
		}
		if r.Indeterminate() {
			t.Fatalf("declared %s must not be indeterminate", code)
		}
		env := QueryResult{Capability: c.Key, Results: []Result{r}}.Envelope()
		if len(env.Errors) != 1 || env.Errors[0].Code != code || env.Errors[0].Indeterminate {
			t.Fatalf("declared %s: envelope errors = %+v", code, env.Errors)
		}
	}
	// The real case: the message keeps the shape it had as provider_error
	// ("capability <key>: provider <addon>: <code>: <message>"), so a consumer
	// that unwraps it from the message (addons#2532) keeps working.
	msg := "No tienes los créditos suficientes para este envío. Agrega créditos y continúa."
	r := runLabel(t, c, failWith(NewCodedError("insufficient_credits", msg)))
	want := "capability commerce.shipping.label: provider skydropx: insufficient_credits: " + msg
	if r.ErrorCode() != "insufficient_credits" || r.Err.Error() != want {
		t.Fatalf("SO-00020: code %q message %q", r.ErrorCode(), r.Err.Error())
	}
}

func TestShippingLabelDoubtfulAnswersStayGeneric(t *testing.T) {
	c := mustContract(t, CommerceShippingLabel)
	// Codes the carriers use for answers where a shipment may exist (or that
	// warehouse reads as doubtful): never declared, still provider_error with
	// the provider's code and message kept in the text.
	for _, code := range []string{
		"provider_unavailable", "http_error", "in_progress", "rate_limited", "bad_response",
		"not_found", "auth_failed", "db_error", "label_failed", "label_unconfirmed",
		"label_incomplete", "indeterminate", "label_ambiguous", "order_conflict", "connector_unconfigured", "quota_exceeded",
	} {
		r := runLabel(t, c, failWith(NewCodedError(code, "detalle")))
		want := "capability commerce.shipping.label: provider skydropx: " + code + ": detalle"
		if r.ErrorCode() != "provider_error" || r.Err.Error() != want {
			t.Fatalf("undeclared %s: code %q message %q", code, r.ErrorCode(), r.Err.Error())
		}
		env := QueryResult{Capability: c.Key, Results: []Result{r}}.Envelope()
		if len(env.Errors) != 1 || env.Errors[0].Code != "provider_error" {
			t.Fatalf("undeclared %s: envelope errors = %+v", code, env.Errors)
		}
	}
	if got := runLabel(t, c, failWith(errors.New("connection reset"))).ErrorCode(); got != "provider_error" {
		t.Fatalf("plain error = %q", got)
	}
	// A guest that reports "timeout" itself: reserved, so never typed.
	if got := runLabel(t, c, failWith(NewCodedError("timeout", "carrier timed out"))).ErrorCode(); got != "provider_error" {
		t.Fatalf("guest timeout = %q", got)
	}
	// The kernel's own deadline stays timeout + indeterminate (reconcile or
	// retry with the same key), whatever the provider would have answered.
	c.Timeout = 10 * time.Millisecond
	r := runLabel(t, c, func(ctx context.Context, _ Call) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, NewCodedError("rejected", "too late")
	})
	if r.ErrorCode() != "timeout" || !r.Indeterminate() {
		t.Fatalf("deadline: code %q indeterminate %v", r.ErrorCode(), r.Indeterminate())
	}
}

// Only commerce.shipping.label declares codes: the other shipping contracts
// keep reporting every provider code as provider_error.
func TestOtherShippingContractsUnchanged(t *testing.T) {
	for _, key := range []string{CommerceShippingRates, CommerceShippingCancel, CommerceShippingTrack} {
		if c := mustContract(t, key); len(c.Errors) != 0 {
			t.Fatalf("%s errors = %v, want none", key, c.Errors)
		}
	}
	c := mustContract(t, CommerceShippingCancel)
	res, err := Executor{}.Run(context.Background(), c, org, "mx", []Provider{{Addon: "skydropx"}},
		map[string]any{"shipment_id": "S1"}, failWith(NewCodedError("rejected", "picked up")))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Results[0].ErrorCode(); got != "provider_error" {
		t.Fatalf("cancel rejected = %q, want provider_error", got)
	}
}
