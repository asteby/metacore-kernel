package capability

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// commerce.payments.status declares not_found: an authoritative "no payment
// for this reference" reaches the consumer typed, while anything ambiguous
// (an undeclared code, a timeout, a plain error) stays generic. A consumer
// that releases an attempt of unknown outcome depends on that difference.
func TestPaymentsStatusNotFoundIsTypedAndAmbiguityIsNot(t *testing.T) {
	c := mustContract(t, CommercePaymentsStatus)
	run := func(err error) Result {
		res, qerr := Executor{}.Run(context.Background(), c, org, "mx", []Provider{{Addon: "mercadopago"}},
			map[string]any{"order_ref": "7c0d6f1e-0000-4000-8000-000000000001"},
			func(context.Context, Call) (json.RawMessage, error) { return nil, err })
		if qerr != nil {
			t.Fatalf("run: %v", qerr)
		}
		return res.Results[0]
	}
	if got := run(NewCodedError(PaymentsStatusNotFound, "no payments")).ErrorCode(); got != "not_found" {
		t.Fatalf("declared not_found = %q", got)
	}
	for name, err := range map[string]error{
		"inconclusive lookup": NewCodedError("not_found_inconclusive", "local index only"),
		"http failure":        NewCodedError("http_error", "connection reset"),
		"plain error":         errors.New("boom"),
	} {
		if got := run(err).ErrorCode(); got != "provider_error" {
			t.Fatalf("%s: code = %q, want provider_error (never not_found)", name, got)
		}
	}
	env := QueryResult{Capability: c.Key, Results: []Result{run(NewCodedError(PaymentsStatusNotFound, ""))}}.Envelope()
	if len(env.Errors) != 1 || env.Errors[0].Code != "not_found" {
		t.Fatalf("envelope errors = %+v", env.Errors)
	}
}
