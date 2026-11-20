package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
)

var errSkip = errors.New("skipped")

func skipf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errSkip, fmt.Sprintf(format, a...))
}

func failf(format string, a ...any) error { return fmt.Errorf(format, a...) }

// env is the per-case helper: unique refs, contract-checked calls.
type env struct {
	ctx      context.Context
	t        Target
	opts     Options
	id       string
	n        int
	restores []func()
}

func (e *env) cleanup() {
	for i := len(e.restores) - 1; i >= 0; i-- {
		e.restores[i]()
	}
}

func (e *env) next(prefix string) string {
	e.n++
	return fmt.Sprintf("conf-%s-%s-%s-%d", e.opts.RunID, strings.ToLower(e.id), prefix, e.n)
}

// call invokes key with input, with the per-call timeout. The input is first
// checked against the capability contract (the suite must itself send valid
// input; negative cases use callRaw).
func (e *env) call(key string, input map[string]any) (json.RawMessage, error) {
	if c, ok := capability.Lookup(key); ok {
		if problems := c.CheckInput(input); len(problems) > 0 {
			return nil, fmt.Errorf("suite bug: input of %s violates the contract: %s", key, strings.Join(problems, "; "))
		}
	}
	return e.callRaw(key, input)
}

func (e *env) callRaw(key string, input map[string]any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(e.ctx, e.opts.Timeout)
	defer cancel()
	return e.t.Invoke(ctx, key, input)
}

// outputOK validates raw against the Output schema of the contract `as`.
func outputOK(as string, raw json.RawMessage) error {
	c, ok := capability.Lookup(as)
	if !ok {
		return nil
	}
	if problems := c.ValidateOutput(raw); len(problems) > 0 {
		return failf("%s output violates the contract: %s", as, strings.Join(problems, "; "))
	}
	return nil
}

type sessionOut struct {
	SessionID   string `json:"session_id"`
	Status      string `json:"status"`
	ProviderRef string `json:"provider_ref"`
	TestMode    *bool  `json:"test_mode"`
}

type order struct {
	OrderRef string
	Key      string
	Amount   string
	Currency string
}

func (e *env) newOrder(amount, currency string) order {
	return order{OrderRef: e.next("order"), Key: e.next("key"), Amount: amount, Currency: currency}
}

func (o order) sessionInput() map[string]any {
	return map[string]any{
		"order_ref":       o.OrderRef,
		"amount":          o.Amount,
		"currency":        o.Currency,
		"return_url":      "https://example.test/return",
		"idempotency_key": o.Key,
	}
}

func (e *env) session(o order) (sessionOut, error) {
	raw, err := e.call(capability.CommercePaymentsSession, o.sessionInput())
	if err != nil {
		return sessionOut{}, err
	}
	if err := outputOK(capability.CommercePaymentsSession, raw); err != nil {
		return sessionOut{}, err
	}
	var s sessionOut
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	return s, nil
}

func decodeOutcome(as string, raw json.RawMessage) (payments.Outcome, error) {
	// status has no Output schema of its own: it answers the confirm output.
	if err := outputOK(capability.CommercePaymentsConfirm, raw); err != nil {
		return payments.Outcome{}, fmt.Errorf("(%s) %w", as, err)
	}
	var o payments.Outcome
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, err
	}
	if !o.Status.Valid() {
		return o, failf("%s: status %q is not in the normalized vocabulary", as, o.Status)
	}
	return o, nil
}

func (e *env) confirm(w Webhook) (payments.Outcome, error) {
	in := map[string]any{"raw_body": w.RawBody}
	var payload any = w.RawBody
	var decoded any
	if json.Unmarshal([]byte(w.RawBody), &decoded) == nil {
		payload = decoded
	}
	in["payload"] = payload
	h := map[string]any{}
	for k, v := range w.Headers {
		h[strings.ToLower(k)] = v
	}
	in["headers"] = h
	if len(w.Query) > 0 {
		q := map[string]any{}
		for k, v := range w.Query {
			q[k] = v
		}
		in["query"] = q
	}
	raw, err := e.call(capability.CommercePaymentsConfirm, in)
	if err != nil {
		return payments.Outcome{}, err
	}
	return decodeOutcome("confirm", raw)
}

func (e *env) status(in map[string]any) (payments.Outcome, error) {
	raw, err := e.call(capability.CommercePaymentsStatus, in)
	if err != nil {
		return payments.Outcome{}, err
	}
	return decodeOutcome("status", raw)
}

type refundOut struct {
	RefundID string `json:"refund_id"`
	Status   string `json:"status"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func (e *env) refund(providerRef, amount, currency, key string) (refundOut, error) {
	raw, err := e.call(capability.CommercePaymentsRefund, map[string]any{
		"provider_ref": providerRef, "amount": amount, "currency": currency, "idempotency_key": key,
	})
	if err != nil {
		return refundOut{}, err
	}
	if err := outputOK(capability.CommercePaymentsRefund, raw); err != nil {
		return refundOut{}, err
	}
	var r refundOut
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	return r, nil
}

// paid is a payment taken to paid through the sandbox.
type paid struct {
	Order   order
	Session sessionOut
	Webhook Webhook
	Outcome payments.Outcome
}

// pay creates a session, settles it as paid and confirms the webhook.
func (e *env) pay(amount, currency string) (paid, error) {
	p := paid{Order: e.newOrder(amount, currency)}
	var err error
	if p.Session, err = e.session(p.Order); err != nil {
		return p, failf("session: %v", err)
	}
	if p.Webhook, err = e.t.Sandbox.Settle(e.ctx, p.Session.SessionID, payments.StatusPaid); err != nil {
		return p, failf("sandbox settle: %v", err)
	}
	if p.Outcome, err = e.confirm(p.Webhook); err != nil {
		return p, failf("confirm of a valid signed webhook: %v", err)
	}
	if p.Outcome.ProviderRef == "" {
		return p, failf("confirm returned no provider_ref")
	}
	return p, nil
}

func (e *env) inject(f Fault) error {
	restore, err := e.t.Sandbox.Inject(e.ctx, f)
	if errors.Is(err, ErrFaultUnsupported) {
		return skipf("sandbox cannot inject %s", f)
	}
	if err != nil {
		return failf("inject %s: %v", f, err)
	}
	e.restores = append(e.restores, restore)
	return nil
}

// indeterminate: the error says the outcome is unknown.
func indeterminate(err error) bool {
	return errors.Is(err, capability.ErrIndeterminate) || errors.Is(err, capability.ErrTimeout) || errors.Is(err, context.DeadlineExceeded)
}

func codeOf(err error) string {
	var c *capability.CodedError
	if errors.As(err, &c) {
		return c.Code
	}
	return ""
}

func sameMoney(a, b string) bool {
	x, err1 := payments.ParseAmount(a)
	y, err2 := payments.ParseAmount(b)
	return err1 == nil && err2 == nil && x.Cmp(y) == 0
}
