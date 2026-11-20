package conformance

import (
	"strings"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
)

var allCases = []Case{
	// ------------------------------------------------------------- session
	{"SES-01", "session output follows the contract and is not paid at creation", caseSession},
	{"SES-02", "same idempotency_key returns the same session (no duplicate charge)", caseSessionIdempotent},
	{"SES-03", "same key with another amount never yields a different session", caseSessionKeyConflict},
	{"SES-04", "distinct orders get distinct sessions", caseSessionDistinct},
	{"SES-05", "the sandbox says so: session test_mode is true", caseSessionTestMode},
	// --------------------------------------------------------------- money
	{"MON-01", "an amount with more decimals than its currency is rejected", caseMoneyNotQuantized},
	{"MON-02", "malformed, zero and negative amounts are rejected", caseMoneyMalformed},
	{"MON-03", "zero-decimal currency (JPY) round-trips whole units", caseMoneyZeroDecimal},
	{"MON-04", "captured amount and currency equal the session's, exactly and quantized", caseMoneyEcho},
	{"MON-05", "refund totals are exact decimals (0.10 + 0.20 = 0.30, no float drift)", caseMoneyExactSum},
	// ------------------------------------------------------------- webhook
	{"WH-01", "a validly signed webhook confirms the payment", caseWebhookValid},
	{"WH-02", "a webhook altered after signing is rejected", caseWebhookTampered},
	{"WH-03", "an unsigned webhook is rejected", caseWebhookUnsigned},
	{"WH-04", "a replayed webhook has the same dedupe id and is ignored by the ledger rule", caseWebhookReplay},
	// -------------------------------------------------------------- status
	{"ST-01", "status re-read agrees with the confirmed webhook (provider is the truth)", caseStatusAgrees},
	{"ST-02", "an unknown reference is an error, never an invented payment", caseStatusUnknown},
	{"ST-03", "failed and expired are reported as final states, valid from pending", caseStatusFinal},
	// -------------------------------------------------------------- refund
	{"RF-01", "partial refund: contract output, exact amount, payment partially_refunded", caseRefundPartial},
	{"RF-02", "refund is idempotent on its key (never refunds twice)", caseRefundIdempotent},
	{"RF-03", "refunding more than the captured amount is rejected", caseRefundOver},
	{"RF-04", "full refund moves the payment to refunded", caseRefundFull},
	{"RF-05", "a payment that was not captured cannot be refunded", caseRefundUncaptured},
	// -------------------------------------------------------- indeterminate
	{"IND-01", "lost session response is an indeterminate error; retry with the same key resolves it", caseIndSession},
	{"IND-02", "lost refund response is an indeterminate error; retry refunds exactly once", caseIndRefund},
	{"IND-03", "provider outage on status is an error, never paid / failed / not_found", caseIndStatusDown},
}

// ------------------------------------------------------------------ session

func caseSession(e *env) error {
	o := e.newOrder("149.90", "MXN")
	s, err := e.session(o)
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.SessionID) == "" {
		return failf("session_id is empty")
	}
	if payments.Status(s.Status) == payments.StatusPaid {
		return failf("a session must not be created already paid (status %q)", s.Status)
	}
	return nil
}

func caseSessionIdempotent(e *env) error {
	o := e.newOrder("50.00", "MXN")
	a, err := e.session(o)
	if err != nil {
		return err
	}
	b, err := e.session(o)
	if err != nil {
		return failf("retry with the same key failed: %v", err)
	}
	if a.SessionID != b.SessionID {
		return failf("same idempotency_key created two sessions: %q and %q", a.SessionID, b.SessionID)
	}
	return nil
}

func caseSessionKeyConflict(e *env) error {
	o := e.newOrder("50.00", "MXN")
	a, err := e.session(o)
	if err != nil {
		return err
	}
	o2 := o
	o2.Amount = "75.00"
	b, err := e.session(o2)
	if err != nil {
		return nil // rejecting the reuse of a key is the strict, correct answer
	}
	if a.SessionID != b.SessionID {
		return failf("same key with another amount created a new session %q", b.SessionID)
	}
	return nil
}

func caseSessionDistinct(e *env) error {
	a, err := e.session(e.newOrder("10.00", "MXN"))
	if err != nil {
		return err
	}
	b, err := e.session(e.newOrder("10.00", "MXN"))
	if err != nil {
		return err
	}
	if a.SessionID == b.SessionID {
		return failf("two different orders share session %q", a.SessionID)
	}
	return nil
}

func caseSessionTestMode(e *env) error {
	s, err := e.session(e.newOrder("10.00", "MXN"))
	if err != nil {
		return err
	}
	if s.TestMode == nil || !*s.TestMode {
		return failf("session did not report test_mode=true: the suite refuses to certify against a non-sandbox")
	}
	return nil
}

// -------------------------------------------------------------------- money

func caseMoneyNotQuantized(e *env) error {
	for _, c := range [][2]string{{"10.005", "MXN"}, {"10.50", "JPY"}} {
		o := e.newOrder(c[0], c[1])
		if _, err := e.callRaw(capability.CommercePaymentsSession, o.sessionInput()); err == nil {
			return failf("%s %s accepted: it has more decimals than the currency", c[0], c[1])
		}
	}
	return nil
}

func caseMoneyMalformed(e *env) error {
	for _, a := range []string{"0", "0.00", "-5.00", "1e3", "1,000.00", "10.5.0", ".5", "abc", ""} {
		o := e.newOrder(a, "MXN")
		if _, err := e.callRaw(capability.CommercePaymentsSession, o.sessionInput()); err == nil {
			return failf("amount %q accepted", a)
		}
	}
	return nil
}

func caseMoneyZeroDecimal(e *env) error {
	p, err := e.pay("1500", "JPY")
	if err != nil {
		return err
	}
	if p.Outcome.Currency != "JPY" || !sameMoney(p.Outcome.Amount, "1500") {
		return failf("JPY 1500 came back as %s %s", p.Outcome.Currency, p.Outcome.Amount)
	}
	if _, err := payments.ParseMoney(p.Outcome.Amount, "JPY"); err != nil {
		return failf("outcome amount is not whole JPY: %v", err)
	}
	return nil
}

func caseMoneyEcho(e *env) error {
	p, err := e.pay("19.99", "MXN")
	if err != nil {
		return err
	}
	return checkEcho(p.Outcome, p.Order)
}

func checkEcho(o payments.Outcome, ord order) error {
	if o.Currency != ord.Currency {
		return failf("currency %q, want %q", o.Currency, ord.Currency)
	}
	if _, err := payments.ParseMoney(o.Amount, o.Currency); err != nil {
		return failf("amount %q is not valid money: %v", o.Amount, err)
	}
	if !sameMoney(o.Amount, ord.Amount) {
		return failf("amount %q, want %q", o.Amount, ord.Amount)
	}
	if o.OrderRef != ord.OrderRef {
		return failf("order_ref %q, want %q", o.OrderRef, ord.OrderRef)
	}
	return nil
}

func caseMoneyExactSum(e *env) error {
	p, err := e.pay("0.30", "MXN")
	if err != nil {
		return err
	}
	for _, amt := range []string{"0.10", "0.20"} {
		r, err := e.refund(p.Outcome.ProviderRef, amt, "MXN", e.next("rkey"))
		if err != nil {
			return failf("refund %s: %v", amt, err)
		}
		if !sameMoney(r.Amount, amt) {
			return failf("refund amount %q, want %q", r.Amount, amt)
		}
	}
	o, err := e.status(map[string]any{"provider_ref": p.Outcome.ProviderRef})
	if err != nil {
		return err
	}
	if _, err := payments.ParseMoney(o.AmountRefunded, "MXN"); err != nil {
		return failf("amount_refunded %q is not valid money: %v", o.AmountRefunded, err)
	}
	if !sameMoney(o.AmountRefunded, "0.30") || o.Status != payments.StatusRefunded {
		return failf("after 0.10 + 0.20 of 0.30: status %s amount_refunded %q, want refunded 0.30", o.Status, o.AmountRefunded)
	}
	return nil
}

// ------------------------------------------------------------------ webhook

func caseWebhookValid(e *env) error {
	p, err := e.pay("100.00", "MXN")
	if err != nil {
		return err
	}
	o := p.Outcome
	if o.Status != payments.StatusPaid {
		return failf("status %s, want paid", o.Status)
	}
	if err := checkEcho(o, p.Order); err != nil {
		return err
	}
	stored := payments.Payment{Status: payments.StatusPending, Amount: p.Order.Amount, Currency: p.Order.Currency}
	if ob := payments.ObserveOutcome(stored, o); ob.Decision != payments.Apply {
		return failf("the ledger would not apply the confirmation: %s %s", ob.Decision, ob.Reason)
	}
	return nil
}

func caseWebhookTampered(e *env) error {
	q := e.newOrder("100.00", "MXN")
	s, err := e.session(q)
	if err != nil {
		return err
	}
	w, err := e.t.Sandbox.Settle(e.ctx, s.SessionID, payments.StatusPaid)
	if err != nil {
		return failf("sandbox settle: %v", err)
	}
	forged := e.t.Sandbox.Forge(w)
	if forged.RawBody == w.RawBody {
		return failf("sandbox Forge did not change the body (sandbox bug)")
	}
	if o, err := e.confirm(forged); err == nil {
		return failf("an altered webhook was accepted as %s %s %s", o.Status, o.Amount, o.Currency)
	}
	return nil
}

func caseWebhookUnsigned(e *env) error {
	s, err := e.session(e.newOrder("100.00", "MXN"))
	if err != nil {
		return err
	}
	w, err := e.t.Sandbox.Settle(e.ctx, s.SessionID, payments.StatusPaid)
	if err != nil {
		return failf("sandbox settle: %v", err)
	}
	if o, err := e.confirm(Webhook{RawBody: w.RawBody}); err == nil {
		return failf("a webhook with no signature headers was accepted as %s", o.Status)
	}
	return nil
}

func caseWebhookReplay(e *env) error {
	p, err := e.pay("80.00", "MXN")
	if err != nil {
		return err
	}
	again, err := e.confirm(p.Webhook)
	if err != nil {
		return failf("a replay of a valid webhook must be answered, not fail: %v", err)
	}
	if again.DedupeEventID() != p.Outcome.DedupeEventID() {
		return failf("replay has another dedupe id (%q vs %q): the ledger would apply it twice", again.DedupeEventID(), p.Outcome.DedupeEventID())
	}
	if again.Status != p.Outcome.Status || !sameMoney(again.Amount, p.Outcome.Amount) {
		return failf("replay changed the outcome: %s %s vs %s %s", again.Status, again.Amount, p.Outcome.Status, p.Outcome.Amount)
	}
	stored := payments.Payment{Status: payments.StatusPaid, Amount: p.Order.Amount, Currency: p.Order.Currency, ProviderRef: p.Outcome.ProviderRef}
	if ob := payments.ObserveOutcome(stored, again); ob.Decision != payments.Ignore {
		return failf("the ledger would not ignore the replay: %s %s", ob.Decision, ob.Reason)
	}
	return nil
}

// ------------------------------------------------------------------- status

func caseStatusAgrees(e *env) error {
	p, err := e.pay("42.00", "MXN")
	if err != nil {
		return err
	}
	for _, in := range []map[string]any{
		{"provider_ref": p.Outcome.ProviderRef},
		{"order_ref": p.Order.OrderRef},
	} {
		o, err := e.status(in)
		if err != nil {
			return failf("status %v: %v", in, err)
		}
		if o.Status != payments.StatusPaid || !sameMoney(o.Amount, p.Order.Amount) || o.Currency != p.Order.Currency {
			return failf("status %v: %s %s %s, webhook said paid %s %s", in, o.Status, o.Amount, o.Currency, p.Order.Amount, p.Order.Currency)
		}
		if o.ProviderRef != p.Outcome.ProviderRef {
			return failf("status %v answers another payment: %q vs %q", in, o.ProviderRef, p.Outcome.ProviderRef)
		}
	}
	return nil
}

func caseStatusUnknown(e *env) error {
	o, err := e.status(map[string]any{"provider_ref": e.next("nope")})
	if err == nil {
		return failf("an unknown provider_ref answered %s", o.Status)
	}
	return nil
}

func caseStatusFinal(e *env) error {
	for _, st := range []payments.Status{payments.StatusFailed, payments.StatusExpired} {
		s, err := e.session(e.newOrder("30.00", "MXN"))
		if err != nil {
			return err
		}
		w, err := e.t.Sandbox.Settle(e.ctx, s.SessionID, st)
		if err != nil {
			if st == payments.StatusFailed {
				return failf("sandbox settle %s: %v", st, err)
			}
			continue // expiry may not be simulable; failed is the required one
		}
		o, err := e.confirm(w)
		if err != nil {
			return failf("confirm %s: %v", st, err)
		}
		if o.Status != st {
			return failf("webhook for %s confirmed as %s", st, o.Status)
		}
		if !o.Status.Final() || !payments.CanTransition(payments.StatusPending, o.Status) {
			return failf("%s is not a final state reachable from pending", o.Status)
		}
		for i := 0; i < 2; i++ { // a final state is stable
			r, err := e.status(map[string]any{"session_id": s.SessionID})
			if err != nil {
				return failf("status after %s: %v", st, err)
			}
			if r.Status != st {
				return failf("status after %s reads %s", st, r.Status)
			}
		}
	}
	return nil
}

// ------------------------------------------------------------------- refund

func refundedOK(e *env, ref, want string, st payments.Status) error {
	o, err := e.status(map[string]any{"provider_ref": ref})
	if err != nil {
		return err
	}
	if !sameMoney(o.AmountRefunded, want) || o.Status != st {
		return failf("payment is %s with amount_refunded %q, want %s %s", o.Status, o.AmountRefunded, st, want)
	}
	return nil
}

func caseRefundPartial(e *env) error {
	p, err := e.pay("20.00", "MXN")
	if err != nil {
		return err
	}
	r, err := e.refund(p.Outcome.ProviderRef, "5.00", "MXN", e.next("rkey"))
	if err != nil {
		return err
	}
	if r.RefundID == "" || !sameMoney(r.Amount, "5.00") || r.Currency != "MXN" {
		return failf("refund answered id %q %s %s", r.RefundID, r.Amount, r.Currency)
	}
	if !validRefundStatus(r.Status) {
		return failf("refund status %q", r.Status)
	}
	if r.Status != "succeeded" {
		return nil // pending / manual: the payment need not reflect it yet
	}
	if err := refundedOK(e, p.Outcome.ProviderRef, "5.00", payments.StatusPartiallyRefunded); err != nil {
		return err
	}
	o, _ := e.status(map[string]any{"provider_ref": p.Outcome.ProviderRef})
	stored := payments.Payment{Status: payments.StatusPaid, Amount: "20.00", Currency: "MXN", ProviderRef: p.Outcome.ProviderRef}
	if ob := payments.ObserveOutcome(stored, o); ob.Decision != payments.Apply {
		return failf("the ledger would not apply the refunded state: %s %s", ob.Decision, ob.Reason)
	}
	return nil
}

func validRefundStatus(s string) bool {
	for _, k := range capability.RefundStatuses {
		if k == s {
			return true
		}
	}
	return false
}

func caseRefundIdempotent(e *env) error {
	p, err := e.pay("20.00", "MXN")
	if err != nil {
		return err
	}
	key := e.next("rkey")
	a, err := e.refund(p.Outcome.ProviderRef, "5.00", "MXN", key)
	if err != nil {
		return err
	}
	b, err := e.refund(p.Outcome.ProviderRef, "5.00", "MXN", key)
	if err != nil {
		return failf("retry with the same key failed: %v", err)
	}
	if a.RefundID != b.RefundID {
		return failf("same key created two refunds: %q and %q", a.RefundID, b.RefundID)
	}
	// the same key with another amount must not refund again either
	if c, err := e.refund(p.Outcome.ProviderRef, "7.00", "MXN", key); err == nil && c.RefundID != a.RefundID {
		return failf("same key with another amount created refund %q", c.RefundID)
	}
	if a.Status == "succeeded" {
		return refundedOK(e, p.Outcome.ProviderRef, "5.00", payments.StatusPartiallyRefunded)
	}
	return nil
}

func caseRefundOver(e *env) error {
	p, err := e.pay("10.00", "MXN")
	if err != nil {
		return err
	}
	ref := p.Outcome.ProviderRef
	if _, err := e.refund(ref, "10.01", "MXN", e.next("rkey")); err == nil {
		return failf("refund of 10.01 on a payment of 10.00 accepted")
	}
	r, err := e.refund(ref, "10.00", "MXN", e.next("rkey"))
	if err != nil {
		return failf("full refund after a rejected over-refund: %v", err)
	}
	if r.Status != "succeeded" {
		return nil
	}
	if _, err := e.refund(ref, "0.01", "MXN", e.next("rkey")); err == nil {
		return failf("a refund on a fully refunded payment was accepted")
	}
	return nil
}

func caseRefundFull(e *env) error {
	p, err := e.pay("12.34", "MXN")
	if err != nil {
		return err
	}
	r, err := e.refund(p.Outcome.ProviderRef, "12.34", "MXN", e.next("rkey"))
	if err != nil {
		return err
	}
	if r.Status != "succeeded" {
		return skipf("provider settles refunds asynchronously (%s)", r.Status)
	}
	return refundedOK(e, p.Outcome.ProviderRef, "12.34", payments.StatusRefunded)
}

func caseRefundUncaptured(e *env) error {
	o := e.newOrder("10.00", "MXN")
	s, err := e.session(o)
	if err != nil {
		return err
	}
	ref := s.ProviderRef
	if ref == "" {
		if st, err := e.status(map[string]any{"session_id": s.SessionID}); err == nil {
			ref = st.ProviderRef
		}
	}
	if ref == "" {
		return skipf("the provider holds no payment reference before the buyer pays")
	}
	if _, err := e.refund(ref, "1.00", "MXN", e.next("rkey")); err == nil {
		return failf("a payment still pending was refunded")
	}
	return nil
}

// ------------------------------------------------------------ indeterminate

func caseIndSession(e *env) error {
	o := e.newOrder("60.00", "MXN")
	if err := e.inject(FaultSessionLostResponse); err != nil {
		return err
	}
	if s, err := e.session(o); err == nil {
		return failf("a lost response was masked as an answer (session %q status %q): an unknown outcome must be an error", s.SessionID, s.Status)
	} else if !indeterminate(err) {
		return failf("a lost response must be indeterminate (capability.ErrIndeterminate / ErrTimeout / deadline), got: %v", err)
	}
	a, err := e.session(o)
	if err != nil {
		return failf("retry with the same key after the timeout: %v", err)
	}
	b, err := e.session(o)
	if err != nil || a.SessionID != b.SessionID {
		return failf("the retry did not converge on one session")
	}
	if st, err := e.status(map[string]any{"session_id": a.SessionID}); err == nil && !st.Status.Open() {
		return failf("a session whose payment is unknown reads %s; it must stay open (pending)", st.Status)
	}
	return nil
}

func caseIndRefund(e *env) error {
	p, err := e.pay("20.00", "MXN")
	if err != nil {
		return err
	}
	key := e.next("rkey")
	if err := e.inject(FaultRefundLostResponse); err != nil {
		return err
	}
	if _, err := e.refund(p.Outcome.ProviderRef, "5.00", "MXN", key); err == nil {
		return failf("a lost refund response was masked as an answer")
	} else if !indeterminate(err) {
		return failf("a lost refund response must be indeterminate, got: %v", err)
	}
	r, err := e.refund(p.Outcome.ProviderRef, "5.00", "MXN", key)
	if err != nil {
		return failf("retry with the same key: %v", err)
	}
	if r.Status != "succeeded" {
		return nil
	}
	return refundedOK(e, p.Outcome.ProviderRef, "5.00", payments.StatusPartiallyRefunded) // once, not twice
}

func caseIndStatusDown(e *env) error {
	p, err := e.pay("20.00", "MXN")
	if err != nil {
		return err
	}
	if err := e.inject(FaultProviderDown); err != nil {
		return err
	}
	o, err := e.status(map[string]any{"provider_ref": p.Outcome.ProviderRef})
	if err == nil {
		return failf("status during an outage answered %s: an unreachable provider is not an answer", o.Status)
	}
	if codeOf(err) == capability.PaymentsStatusNotFound {
		return failf("an outage was reported as not_found, which is authoritative: the consumer may release the attempt")
	}
	return nil
}
