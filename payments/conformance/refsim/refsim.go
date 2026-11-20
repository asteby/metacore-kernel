// Package refsim is the reference payments provider of CONF-PAYMENTS@1: an
// in-memory simulator of a commerce.payments.* provider (session, confirm,
// status, refund) with HMAC-SHA256 signed webhooks, that passes the whole
// suite. It is the executable definition of "conforming" and a starting point
// for a provider's own simulator.
//
// Quirks switch ONE defect on, so the suite can be shown to fail a broken
// provider (see conformance_test.go). A quirk-free Sim is conforming.
package refsim

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/asteby/metacore-kernel/payments/conformance"
)

// Quirk is a deliberate defect.
type Quirk string

const (
	QuirkNoSignatureCheck     Quirk = "no_signature_check"     // accepts any webhook
	QuirkNoSessionIdempotency Quirk = "no_session_idempotency" // a retry creates another session
	QuirkAcceptSubCents       Quirk = "accept_sub_cents"       // 10.005 MXN is money
	QuirkAcceptZero           Quirk = "accept_zero"            // 0 / negative amounts pass
	QuirkAssumeFailedOnLost   Quirk = "assume_failed_on_lost"  // timeout turned into a failed session
	QuirkNotFoundOnOutage     Quirk = "not_found_on_outage"    // outage reported as not_found
	QuirkPaidOnOutage         Quirk = "paid_on_outage"         // outage assumed paid
	QuirkNoRefundIdempotency  Quirk = "no_refund_idempotency"  // retry refunds twice
	QuirkNoOverRefundCheck    Quirk = "no_over_refund_check"   // refunds beyond the capture
	QuirkFloatRefundTotals    Quirk = "float_refund_totals"    // sums refunds as float64
	QuirkFreshEventIDOnReplay Quirk = "fresh_event_id_replay"  // replay gets a new event id
	QuirkLiveMode             Quirk = "live_mode"              // talks to production (test_mode=false)
	QuirkRefundUncaptured     Quirk = "refund_uncaptured"      // refunds a pending payment
)

type session struct {
	id, order, key, amount, currency, ref string
	status                                payments.Status
	refunded                              payments.Amount
	refundedF                             float64
	refunds                               map[string]*refund
}

type refund struct {
	id, amount string
}

// Sim is the simulator; it implements conformance.Sandbox and, through
// Invoke, conformance.Invoker.
type Sim struct {
	mu      sync.Mutex
	secret  []byte
	quirks  map[Quirk]bool
	n       int
	byID    map[string]*session
	byKey   map[string]*session
	byRef   map[string]*session
	byOrder map[string]*session
	faults  map[conformance.Fault]bool
}

// New returns a Sim with the given defects.
func New(quirks ...Quirk) *Sim {
	s := &Sim{
		secret: []byte("refsim-secret"),
		quirks: map[Quirk]bool{},
		byID:   map[string]*session{}, byKey: map[string]*session{},
		byRef: map[string]*session{}, byOrder: map[string]*session{},
		faults: map[conformance.Fault]bool{},
	}
	for _, q := range quirks {
		s.quirks[q] = true
	}
	return s
}

// Target is the Sim as a conformance target.
func (s *Sim) Target(name string) conformance.Target {
	return conformance.Target{Name: name, Invoke: s.Invoke, Sandbox: s}
}

func (s *Sim) id(prefix string) string { s.n++; return fmt.Sprintf("%s_%d", prefix, s.n) }

func coded(code, msg string) error { return capability.NewCodedError(code, msg) }

func str(in map[string]any, k string) string { v, _ := in[k].(string); return v }

// Invoke implements conformance.Invoker.
func (s *Sim) Invoke(ctx context.Context, key string, in map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out map[string]any
	var err error
	switch key {
	case capability.CommercePaymentsSession:
		out, err = s.doSession(in)
	case capability.CommercePaymentsConfirm:
		out, err = s.doConfirm(in)
	case capability.CommercePaymentsStatus:
		out, err = s.doStatus(in)
	case capability.CommercePaymentsRefund:
		out, err = s.doRefund(in)
	default:
		return nil, coded("unsupported", key)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func lost() error {
	return fmt.Errorf("%w: %w: refsim lost the response", capability.ErrTimeout, capability.ErrIndeterminate)
}

func (s *Sim) checkMoney(amount, currency string) (payments.Amount, error) {
	a, err := payments.ParseAmount(amount)
	if err != nil {
		return a, coded("invalid_input", err.Error())
	}
	if !capability.ValidCurrency(currency) {
		return a, coded("invalid_input", "currency")
	}
	if !s.quirks[QuirkAcceptSubCents] {
		if err := payments.CheckQuantized(a, currency); err != nil {
			return a, coded("invalid_input", err.Error())
		}
	}
	if a.Sign() <= 0 && !s.quirks[QuirkAcceptZero] {
		return a, coded("invalid_input", "amount must be positive")
	}
	return a, nil
}

func (s *Sim) sessionOut(x *session) map[string]any {
	return map[string]any{
		"session_id": x.id, "status": "pending", "action_type": "redirect",
		"redirect_url": "https://sandbox.refsim.test/pay/" + x.id,
		"provider_ref": x.ref, "test_mode": !s.quirks[QuirkLiveMode],
	}
}

func (s *Sim) doSession(in map[string]any) (map[string]any, error) {
	key := str(in, "idempotency_key")
	if strings.TrimSpace(key) == "" || str(in, "order_ref") == "" {
		return nil, coded("invalid_input", "idempotency_key and order_ref are required")
	}
	if _, err := s.checkMoney(str(in, "amount"), str(in, "currency")); err != nil {
		return nil, err
	}
	if x, ok := s.byKey[key]; ok && !s.quirks[QuirkNoSessionIdempotency] {
		if x.amount != str(in, "amount") || x.currency != str(in, "currency") || x.order != str(in, "order_ref") {
			return nil, coded("idempotency_conflict", "key reused with other parameters")
		}
		return s.sessionOut(x), nil
	}
	x := &session{id: s.id("ses"), order: str(in, "order_ref"), key: key, amount: str(in, "amount"),
		currency: str(in, "currency"), ref: s.id("pay"), status: payments.StatusPending, refunds: map[string]*refund{}}
	s.byID[x.id], s.byKey[key], s.byRef[x.ref], s.byOrder[x.order] = x, x, x, x
	if s.faults[conformance.FaultSessionLostResponse] {
		delete(s.faults, conformance.FaultSessionLostResponse)
		if s.quirks[QuirkAssumeFailedOnLost] {
			o := s.sessionOut(x)
			o["status"] = "failed"
			return o, nil
		}
		return nil, lost()
	}
	return s.sessionOut(x), nil
}

// hook is the body of a notification.
type hook struct {
	EventID     string `json:"event_id"`
	OrderRef    string `json:"order_ref"`
	SessionID   string `json:"session_id"`
	ProviderRef string `json:"provider_ref"`
	Status      string `json:"status"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
}

func (s *Sim) sign(body string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Sim) doConfirm(in map[string]any) (map[string]any, error) {
	body := str(in, "raw_body")
	hdr, _ := in["headers"].(map[string]any)
	sig, _ := hdr["x-signature"].(string)
	if !s.quirks[QuirkNoSignatureCheck] {
		if sig == "" {
			return nil, coded("invalid_signature", "missing signature")
		}
		if !hmac.Equal([]byte(sig), []byte(s.sign(body))) {
			return nil, coded("invalid_signature", "bad signature")
		}
	}
	var h hook
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		return nil, coded("invalid_input", "body")
	}
	ev := h.EventID
	if s.quirks[QuirkFreshEventIDOnReplay] {
		ev = s.id("evt")
	}
	return map[string]any{
		"order_ref": h.OrderRef, "status": h.Status, "amount": h.Amount, "currency": h.Currency,
		"provider_ref": h.ProviderRef, "event_id": ev, "kind": "payment", "session_id": h.SessionID,
		"test_mode": !s.quirks[QuirkLiveMode],
	}, nil
}

func (s *Sim) outcome(x *session) map[string]any {
	o := map[string]any{
		"order_ref": x.order, "status": string(x.status), "amount": x.amount, "currency": x.currency,
		"provider_ref": x.ref, "kind": "payment", "session_id": x.id, "test_mode": !s.quirks[QuirkLiveMode],
	}
	if s.quirks[QuirkFloatRefundTotals] && x.refundedF > 0 {
		o["amount_refunded"] = strconv.FormatFloat(x.refundedF, 'f', -1, 64)
	} else if x.refunded.Sign() > 0 {
		o["amount_refunded"] = x.refunded.String()
	}
	return o
}

func (s *Sim) doStatus(in map[string]any) (map[string]any, error) {
	if s.faults[conformance.FaultProviderDown] {
		switch {
		case s.quirks[QuirkNotFoundOnOutage]:
			return nil, coded(capability.PaymentsStatusNotFound, "no payment")
		case s.quirks[QuirkPaidOnOutage]:
			return map[string]any{"order_ref": str(in, "order_ref"), "status": "paid", "amount": "0.01", "currency": "MXN", "provider_ref": str(in, "provider_ref")}, nil
		}
		return nil, errors.New("refsim: provider API unreachable")
	}
	var x *session
	switch {
	case str(in, "provider_ref") != "":
		x = s.byRef[str(in, "provider_ref")]
	case str(in, "session_id") != "":
		x = s.byID[str(in, "session_id")]
	case str(in, "order_ref") != "":
		x = s.byOrder[str(in, "order_ref")]
	}
	if x == nil {
		return nil, coded(capability.PaymentsStatusNotFound, "no payment for the reference")
	}
	return s.outcome(x), nil
}

func (s *Sim) doRefund(in map[string]any) (map[string]any, error) {
	key := str(in, "idempotency_key")
	x := s.byRef[str(in, "provider_ref")]
	if x == nil {
		return nil, coded("not_found", "payment")
	}
	amt, err := s.checkMoney(str(in, "amount"), str(in, "currency"))
	if err != nil {
		return nil, err
	}
	if str(in, "currency") != x.currency {
		return nil, coded("invalid_input", "currency mismatch")
	}
	if r, ok := x.refunds[key]; ok && !s.quirks[QuirkNoRefundIdempotency] {
		if r.amount != str(in, "amount") {
			return nil, coded("idempotency_conflict", "key reused with another amount")
		}
		return s.refundOut(x, r), nil
	}
	if !x.status.Captured() && !s.quirks[QuirkRefundUncaptured] {
		return nil, coded("invalid_state", "payment not captured")
	}
	total := payments.MustAmount(x.amount)
	if !s.quirks[QuirkNoOverRefundCheck] && x.refunded.Add(amt).Cmp(total) > 0 {
		return nil, coded("invalid_amount", "refund exceeds the captured amount")
	}
	r := &refund{id: s.id("ref"), amount: str(in, "amount")}
	x.refunds[key] = r
	x.refunded = x.refunded.Add(amt)
	f, _ := strconv.ParseFloat(r.amount, 64)
	x.refundedF += f
	if x.refunded.Cmp(total) >= 0 {
		x.status = payments.StatusRefunded
	} else if x.status.Captured() {
		x.status = payments.StatusPartiallyRefunded
	}
	if s.quirks[QuirkFloatRefundTotals] {
		// float64 drift: 0.1 + 0.2 != 0.3, so the payment never reaches refunded
		if x.refundedF != mustFloat(x.amount) && x.status == payments.StatusRefunded {
			x.status = payments.StatusPartiallyRefunded
		}
	}
	if s.faults[conformance.FaultRefundLostResponse] {
		delete(s.faults, conformance.FaultRefundLostResponse)
		return nil, lost()
	}
	return s.refundOut(x, r), nil
}

func mustFloat(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

func (s *Sim) refundOut(x *session, r *refund) map[string]any {
	return map[string]any{"refund_id": r.id, "status": "succeeded", "amount": r.amount, "currency": x.currency, "provider_ref": x.ref}
}

// Settle implements conformance.Sandbox.
func (s *Sim) Settle(_ context.Context, sessionID string, st payments.Status) (conformance.Webhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.byID[sessionID]
	if x == nil {
		return conformance.Webhook{}, fmt.Errorf("refsim: unknown session %q", sessionID)
	}
	switch st {
	case payments.StatusPaid, payments.StatusFailed, payments.StatusExpired, payments.StatusCancelled:
	default:
		return conformance.Webhook{}, fmt.Errorf("refsim: cannot settle as %s", st)
	}
	x.status = st
	b, _ := json.Marshal(hook{EventID: s.id("evt"), OrderRef: x.order, SessionID: x.id, ProviderRef: x.ref,
		Status: string(st), Amount: x.amount, Currency: x.currency})
	body := string(b)
	return conformance.Webhook{RawBody: body, Headers: map[string]string{"x-signature": s.sign(body)}}, nil
}

// Forge implements conformance.Sandbox: the amount is raised, the signature kept.
func (s *Sim) Forge(w conformance.Webhook) conformance.Webhook {
	var h hook
	_ = json.Unmarshal([]byte(w.RawBody), &h)
	h.Amount = "999999.00"
	b, _ := json.Marshal(h)
	w.RawBody = string(b)
	return w
}

// Inject implements conformance.Sandbox.
func (s *Sim) Inject(_ context.Context, f conformance.Fault) (func(), error) {
	switch f {
	case conformance.FaultSessionLostResponse, conformance.FaultRefundLostResponse, conformance.FaultProviderDown:
	default:
		return nil, conformance.ErrFaultUnsupported
	}
	s.mu.Lock()
	s.faults[f] = true
	s.mu.Unlock()
	return func() { s.mu.Lock(); delete(s.faults, f); s.mu.Unlock() }, nil
}
