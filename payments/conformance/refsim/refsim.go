// Package refsim is the reference payments provider of CONF-PAYMENTS@1: an
// in-memory simulator of a commerce.payments.* provider (session, confirm,
// status, refund) with timestamped HMAC-SHA256 signed webhooks, that passes
// the whole suite. It is the executable definition of "conforming" and a
// starting point for a provider's own simulator.
//
// Quirks switch ONE defect on, so the suite can be shown to fail a broken
// provider (see conformance_test.go). AsyncRefunds is not a defect: it is the
// legitimate behavior of providers that settle refunds later. A quirk-free Sim
// is conforming.
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
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/asteby/metacore-kernel/payments/conformance"
)

// Quirk is a deliberate defect (or, for AsyncRefunds, a legitimate mode).
type Quirk string

const (
	AsyncRefunds Quirk = "async_refunds" // NOT a defect: refunds answer pending and settle on the next read

	QuirkNoSignatureCheck        Quirk = "no_signature_check"        // accepts any webhook
	QuirkNoTimestampCheck        Quirk = "no_timestamp_check"        // accepts stale (replayed) webhooks
	QuirkNoSessionIdempotency    Quirk = "no_session_idempotency"    // a retry creates another session
	QuirkDoubleSessionAfterLost  Quirk = "double_session_after_lost" // after a lost response the retry creates a 2nd session, then is idempotent
	QuirkKeyReuseMutatesAmount   Quirk = "key_reuse_mutates_amount"  // same key, other amount: same session, new amount
	QuirkAcceptSubCents          Quirk = "accept_sub_cents"          // 10.005 MXN is money
	QuirkAcceptZero              Quirk = "accept_zero"               // 0 / negative amounts pass
	QuirkTwoDecimalsOnly         Quirk = "two_decimals_only"         // every currency has 2 decimals
	QuirkFloatAmounts            Quirk = "float_amounts"             // amounts go through float64
	QuirkAcceptLowercaseCcy      Quirk = "accept_lowercase_currency" // "mxn" is accepted
	QuirkRefundCurrencyUnchecked Quirk = "refund_currency_unchecked"
	QuirkEchoSessionAmount       Quirk = "echo_session_amount"      // reports the session amount, not what was charged
	QuirkAssumeFailedOnLost      Quirk = "assume_failed_on_lost"    // timeout turned into a failed session
	QuirkNotFoundOnOutage        Quirk = "not_found_on_outage"      // outage reported as not_found
	QuirkPaidOnOutage            Quirk = "paid_on_outage"           // outage assumed paid
	QuirkNoRecoveryAfterOutage   Quirk = "no_recovery_after_outage" // status stays broken after the outage
	QuirkNoRefundIdempotency     Quirk = "no_refund_idempotency"    // retry refunds twice
	QuirkDoubleRefundAfterLost   Quirk = "double_refund_after_lost" // after a lost response the retry refunds again
	QuirkNoOverRefundCheck       Quirk = "no_over_refund_check"     // refunds beyond the capture
	QuirkPendingRefundsIgnored   Quirk = "pending_refunds_ignored"  // pending refunds do not reserve the remainder
	QuirkRefundPendingForever    Quirk = "refund_pending_forever"   // refund answers pending and never settles
	QuirkFloatRefundTotals       Quirk = "float_refund_totals"      // sums refunds as float64
	QuirkFreshEventIDOnReplay    Quirk = "fresh_event_id_replay"    // replay gets a new event id
	QuirkLateWebhookRegresses    Quirk = "late_webhook_regresses"   // a late pending moves paid back to pending
	QuirkNoAlsoCaptured          Quirk = "no_also_captured"         // double charges are not flagged
	QuirkLiveMode                Quirk = "live_mode"                // talks to production (session test_mode=false)
	QuirkLiveStatus              Quirk = "live_status"              // status/confirm say test_mode=false, session says true
	QuirkRefundUncaptured        Quirk = "refund_uncaptured"        // refunds a pending payment
)

// tolerance of the webhook timestamp.
const tolerance = 5 * time.Minute

type session struct {
	id, order, key, amount, currency, ref string
	charged                               string // what was really charged (set by Settle)
	status                                payments.Status
	refunded                              payments.Amount
	reserved                              payments.Amount // async refunds not yet applied
	refundedF                             float64
	refunds                               map[string]*refund
	refundCount                           int
}

type refund struct {
	id, amount string
	pending    bool
}

// Sim is the simulator; it implements conformance.Sandbox and, through
// Invoke, conformance.Invoker.
type Sim struct {
	mu     sync.Mutex
	secret []byte
	quirks map[Quirk]bool
	// Now is the clock (tests may replace it).
	Now     func() time.Time
	n       int
	byID    map[string]*session
	byKey   map[string]*session
	byRef   map[string]*session
	byOrder map[string][]*session
	faults  map[conformance.Fault]bool
}

// New returns a Sim with the given defects.
func New(quirks ...Quirk) *Sim {
	s := &Sim{
		secret: []byte("refsim-secret"), quirks: map[Quirk]bool{}, Now: time.Now,
		byID: map[string]*session{}, byKey: map[string]*session{}, byRef: map[string]*session{},
		byOrder: map[string][]*session{}, faults: map[conformance.Fault]bool{},
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
func (s *Sim) Invoke(_ context.Context, key string, in map[string]any) (json.RawMessage, error) {
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
	ccyOK := capability.ValidCurrency(currency)
	if s.quirks[QuirkAcceptLowercaseCcy] && len(currency) == 3 && strings.ToUpper(currency) != currency {
		ccyOK = true
	}
	if !ccyOK {
		return a, coded("invalid_input", "currency")
	}
	if !s.quirks[QuirkAcceptSubCents] {
		if s.quirks[QuirkTwoDecimalsOnly] {
			if err := payments.CheckQuantized(a, "MXN"); err != nil {
				return a, coded("invalid_input", err.Error())
			}
		} else if err := payments.CheckQuantized(a, strings.ToUpper(currency)); err != nil {
			return a, coded("invalid_input", err.Error())
		}
	}
	if a.Sign() <= 0 && !s.quirks[QuirkAcceptZero] {
		return a, coded("invalid_input", "amount must be positive")
	}
	return a, nil
}

// money is how an amount is written on output.
func (s *Sim) money(a string) string {
	if s.quirks[QuirkFloatAmounts] {
		f, _ := strconv.ParseFloat(a, 64)
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return a
}

func (s *Sim) testMode(live bool) bool {
	return !(live && s.quirks[QuirkLiveMode]) && !(!live && s.quirks[QuirkLiveStatus])
}

func (s *Sim) sessionOut(x *session) map[string]any {
	return map[string]any{
		"session_id": x.id, "status": "pending", "action_type": "redirect",
		"redirect_url": "https://sandbox.refsim.test/pay/" + x.id,
		"provider_ref": x.ref, "test_mode": !s.quirks[QuirkLiveMode],
	}
}

func (s *Sim) doSession(in map[string]any) (map[string]any, error) {
	key := strings.TrimSpace(str(in, "idempotency_key"))
	if key == "" || str(in, "order_ref") == "" {
		return nil, coded("invalid_input", "idempotency_key and order_ref are required")
	}
	if _, err := s.checkMoney(str(in, "amount"), str(in, "currency")); err != nil {
		return nil, err
	}
	if x, ok := s.byKey[key]; ok && !s.quirks[QuirkNoSessionIdempotency] {
		if x.amount != str(in, "amount") || x.currency != str(in, "currency") || x.order != str(in, "order_ref") {
			if s.quirks[QuirkKeyReuseMutatesAmount] {
				x.amount = str(in, "amount")
				return s.sessionOut(x), nil
			}
			return nil, coded("idempotency_conflict", "key reused with other parameters")
		}
		return s.sessionOut(x), nil
	}
	x := &session{id: s.id("ses"), order: str(in, "order_ref"), key: key, amount: str(in, "amount"),
		currency: str(in, "currency"), ref: s.id("pay"), status: payments.StatusPending, refunds: map[string]*refund{}}
	s.byID[x.id], s.byRef[x.ref] = x, x
	s.byOrder[x.order] = append(s.byOrder[x.order], x)
	s.byKey[key] = x
	if s.faults[conformance.FaultSessionLostResponse] {
		delete(s.faults, conformance.FaultSessionLostResponse)
		if s.quirks[QuirkAssumeFailedOnLost] {
			o := s.sessionOut(x)
			o["status"] = "failed"
			return o, nil
		}
		if s.quirks[QuirkDoubleSessionAfterLost] {
			delete(s.byKey, key) // the retry will not find it
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

func (s *Sim) sign(ts, body string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Sim) doConfirm(in map[string]any) (map[string]any, error) {
	body := str(in, "raw_body")
	hdr, _ := in["headers"].(map[string]any)
	sig, _ := hdr["x-signature"].(string)
	tsv, _ := hdr["x-timestamp"].(string)
	if !s.quirks[QuirkNoSignatureCheck] {
		if sig == "" || tsv == "" {
			return nil, coded("invalid_signature", "missing signature")
		}
		if !hmac.Equal([]byte(sig), []byte(s.sign(tsv, body))) {
			return nil, coded("invalid_signature", "bad signature")
		}
		if !s.quirks[QuirkNoTimestampCheck] {
			sec, err := strconv.ParseInt(tsv, 10, 64)
			if err != nil || s.Now().Sub(time.Unix(sec, 0)).Abs() > tolerance {
				return nil, coded("stale_webhook", "timestamp outside the tolerance")
			}
		}
	}
	var h hook
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		return nil, coded("invalid_input", "body")
	}
	x := s.byID[h.SessionID]
	if x == nil {
		return nil, coded("unknown_session", "the notification is about no known session")
	}
	if h.Currency != x.currency {
		return nil, coded("currency_mismatch", "notification currency differs from the session's")
	}
	if want := s.chargedOf(x); h.Amount != want && h.Status != "pending" {
		return nil, coded("amount_mismatch", "notification amount differs from what was charged")
	}
	if s.quirks[QuirkLateWebhookRegresses] && x.status.Captured() {
		x.status = payments.Status(h.Status)
	}
	ev := h.EventID
	if s.quirks[QuirkFreshEventIDOnReplay] {
		ev = s.id("evt")
	}
	amount := h.Amount
	if s.quirks[QuirkEchoSessionAmount] {
		amount = x.amount
	}
	out := map[string]any{
		"order_ref": h.OrderRef, "status": h.Status, "amount": s.money(amount), "currency": h.Currency,
		"provider_ref": h.ProviderRef, "event_id": ev, "kind": "payment", "session_id": h.SessionID,
		"test_mode": s.testMode(false),
	}
	if !s.quirks[QuirkNoAlsoCaptured] {
		if others := s.alsoCaptured(x); len(others) > 0 {
			out["also_captured"] = others
		}
	}
	return out, nil
}

func (s *Sim) chargedOf(x *session) string {
	if x.charged != "" {
		return x.charged
	}
	return x.amount
}

func (s *Sim) alsoCaptured(x *session) []string {
	var out []string
	for _, o := range s.byOrder[x.order] {
		if o != x && o.status.Captured() && x.status.Captured() {
			out = append(out, o.ref)
		}
	}
	return out
}

// settlePending applies the async refunds reserved so far.
func (s *Sim) settlePending(x *session) {
	if x.reserved.Sign() == 0 {
		return
	}
	x.refunded = x.refunded.Add(x.reserved)
	x.reserved = payments.Amount{}
	for _, r := range x.refunds {
		r.pending = false
	}
	s.reconcile(x)
}

func (s *Sim) reconcile(x *session) {
	total := payments.MustAmount(s.chargedOf(x))
	switch {
	case x.refunded.Cmp(total) >= 0:
		x.status = payments.StatusRefunded
	case x.refunded.Sign() > 0 && x.status.Captured():
		x.status = payments.StatusPartiallyRefunded
	}
	if s.quirks[QuirkFloatRefundTotals] && x.status == payments.StatusRefunded && x.refundedF != mustFloat(s.chargedOf(x)) {
		x.status = payments.StatusPartiallyRefunded // float64 drift: 0.1 + 0.2 != 0.3
	}
}

func (s *Sim) outcome(x *session) map[string]any {
	s.settlePending(x)
	amount := s.chargedOf(x)
	if s.quirks[QuirkEchoSessionAmount] {
		amount = x.amount
	}
	o := map[string]any{
		"order_ref": x.order, "status": string(x.status), "amount": s.money(amount), "currency": x.currency,
		"provider_ref": x.ref, "kind": "payment", "session_id": x.id, "test_mode": s.testMode(false),
	}
	if s.quirks[QuirkFloatRefundTotals] && x.refundedF > 0 {
		o["amount_refunded"] = strconv.FormatFloat(x.refundedF, 'f', -1, 64)
	} else if x.refunded.Sign() > 0 {
		o["amount_refunded"] = x.refunded.String()
	}
	if !s.quirks[QuirkNoAlsoCaptured] {
		if others := s.alsoCaptured(x); len(others) > 0 {
			o["also_captured"] = others
		}
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
		for _, c := range s.byOrder[str(in, "order_ref")] {
			if x == nil || (c.status.Captured() && !x.status.Captured()) {
				x = c
			}
		}
	}
	if x == nil {
		return nil, coded(capability.PaymentsStatusNotFound, "no payment for the reference")
	}
	return s.outcome(x), nil
}

func (s *Sim) doRefund(in map[string]any) (map[string]any, error) {
	key := strings.TrimSpace(str(in, "idempotency_key"))
	x := s.byRef[str(in, "provider_ref")]
	if x == nil {
		return nil, coded("not_found", "payment")
	}
	amt, err := s.checkMoney(str(in, "amount"), str(in, "currency"))
	if err != nil {
		return nil, err
	}
	if str(in, "currency") != x.currency && !s.quirks[QuirkRefundCurrencyUnchecked] {
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
	total := payments.MustAmount(s.chargedOf(x))
	used := x.refunded
	if !s.quirks[QuirkPendingRefundsIgnored] {
		used = used.Add(x.reserved)
	}
	if !s.quirks[QuirkNoOverRefundCheck] && used.Add(amt).Cmp(total) > 0 {
		return nil, coded("invalid_amount", "refund exceeds what is left to refund")
	}
	r := &refund{id: s.id("ref"), amount: str(in, "amount")}
	x.refunds[key] = r
	x.refundCount++
	f, _ := strconv.ParseFloat(r.amount, 64)
	x.refundedF += f
	switch {
	case s.quirks[QuirkRefundPendingForever]:
		r.pending = true
	case s.quirks[AsyncRefunds]:
		r.pending = true
		x.reserved = x.reserved.Add(amt)
	default:
		x.refunded = x.refunded.Add(amt)
		s.reconcile(x)
	}
	if s.faults[conformance.FaultRefundLostResponse] {
		delete(s.faults, conformance.FaultRefundLostResponse)
		if s.quirks[QuirkDoubleRefundAfterLost] {
			delete(x.refunds, key)
		}
		return nil, lost()
	}
	return s.refundOut(x, r), nil
}

func mustFloat(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

func (s *Sim) refundOut(x *session, r *refund) map[string]any {
	st := "succeeded"
	if r.pending {
		st = "pending"
	}
	return map[string]any{"refund_id": r.id, "status": st, "amount": r.amount, "currency": x.currency, "provider_ref": x.ref}
}

func (s *Sim) signedHook(x *session, status payments.Status, amount string, at time.Time) conformance.Webhook {
	b, _ := json.Marshal(hook{EventID: s.id("evt"), OrderRef: x.order, SessionID: x.id, ProviderRef: x.ref,
		Status: string(status), Amount: amount, Currency: x.currency})
	body := string(b)
	ts := strconv.FormatInt(at.Unix(), 10)
	return conformance.Webhook{RawBody: body, Headers: map[string]string{"x-signature": s.sign(ts, body), "x-timestamp": ts}}
}

// Settle implements conformance.Sandbox.
func (s *Sim) Settle(_ context.Context, spec conformance.SettleSpec) (conformance.Webhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.byID[spec.SessionID]
	if x == nil {
		return conformance.Webhook{}, fmt.Errorf("refsim: unknown session %q", spec.SessionID)
	}
	switch spec.Status {
	case payments.StatusPaid, payments.StatusFailed, payments.StatusExpired, payments.StatusCancelled:
		if x.status.Captured() && spec.Status != payments.StatusPaid {
			return conformance.Webhook{}, fmt.Errorf("refsim: a %s payment cannot become %s", x.status, spec.Status)
		}
		if x.status != payments.StatusPending && !x.status.Captured() && x.status != spec.Status {
			return conformance.Webhook{}, fmt.Errorf("refsim: a %s payment is final", x.status)
		}
		if spec.Status == payments.StatusPaid && !x.status.Captured() {
			x.charged = spec.Amount
		}
		x.status = spec.Status
	case payments.StatusPending: // a late notification: the state does not move
	default:
		return conformance.Webhook{}, fmt.Errorf("refsim: cannot settle as %s", spec.Status)
	}
	amount := s.chargedOf(x)
	if spec.Status == payments.StatusPending && spec.Amount != "" {
		amount = spec.Amount
	}
	return s.signedHook(x, spec.Status, amount, s.Now()), nil
}

// Age implements conformance.Sandbox.
func (s *Sim) Age(_ context.Context, w conformance.Webhook, by time.Duration) (conformance.Webhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := strconv.FormatInt(s.Now().Add(-by).Unix(), 10)
	return conformance.Webhook{RawBody: w.RawBody, Query: w.Query,
		Headers: map[string]string{"x-signature": s.sign(ts, w.RawBody), "x-timestamp": ts}}, nil
}

// Effects implements conformance.Sandbox.
func (s *Sim) Effects(_ context.Context, orderRef string) (conformance.Effects, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ef conformance.Effects
	for _, x := range s.byOrder[orderRef] {
		ef.Sessions++
		ef.Refunds += x.refundCount
	}
	return ef, nil
}

// Inject implements conformance.Sandbox.
func (s *Sim) Inject(_ context.Context, f conformance.Fault) (func(), error) {
	switch f {
	case conformance.FaultSessionLostResponse, conformance.FaultRefundLostResponse, conformance.FaultProviderDown:
	default:
		return nil, conformance.ErrUnsupported
	}
	s.mu.Lock()
	s.faults[f] = true
	s.mu.Unlock()
	return func() {
		if f == conformance.FaultProviderDown && s.quirks[QuirkNoRecoveryAfterOutage] {
			return // broken provider: stays down
		}
		s.mu.Lock()
		delete(s.faults, f)
		s.mu.Unlock()
	}, nil
}
