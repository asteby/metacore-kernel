package conformance

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
)

var allCases = []Case{
	// ------------------------------------------------------------- session
	{"SES-01", "session output follows the contract and is not paid at creation", caseSession},
	{"SES-02", "same idempotency_key returns the same session and creates exactly one remote session", caseSessionIdempotent},
	{"SES-03", "same key with another amount: typed idempotency conflict, or the identical session with its amount intact", caseSessionKeyConflict},
	{"SES-04", "distinct orders get distinct sessions", caseSessionDistinct},
	{"SES-05", "the sandbox says so: session test_mode is true", caseSessionTestMode},
	// --------------------------------------------------------------- money
	{"MON-01", "an amount with more decimals than its currency is rejected", caseMoneyNotQuantized},
	{"MON-02", "malformed, zero and negative amounts are rejected", caseMoneyMalformed},
	{"MON-03", "zero-decimal currency (JPY) round-trips whole units", caseMoneyZeroDecimal},
	{"MON-04", "the provider reports what was really charged (19.50 on a 19.99 session), so the mismatch is detectable", caseMoneyReportsCharged},
	{"MON-05", "refund totals are exact decimals (0.10 + 0.20 = 0.30, no float drift)", caseMoneyExactSum},
	{"MON-06", "three-decimal currencies (KWD): 10.005 accepted, 10.0005 rejected", caseMoneyThreeDecimals},
	{"MON-07", "amounts beyond float64 / int64 precision are exact or rejected, never rounded", caseMoneyBig},
	{"MON-08", "invalid or lower-case currency is rejected", caseMoneyBadCurrency},
	{"MON-09", "a refund in another currency is rejected", caseMoneyRefundCurrency},
	// ------------------------------------------------------------- webhook
	{"WH-01", "a validly signed webhook confirms the payment", caseWebhookValid},
	{"WH-02", "a webhook with a corrupted signature, or altered after signing, is rejected by its signature (or re-read to the genuine state)", caseWebhookTampered},
	{"WH-03", "a webhook without signature headers is rejected (or re-read to the genuine state)", caseWebhookUnsigned},
	{"WH-04", "a replayed webhook has the same dedupe id and is ignored by the ledger rule", caseWebhookReplay},
	{"WH-05", "a correctly signed but stale webhook (old timestamp) is rejected", caseWebhookStale},
	{"WH-06", "a late pending notification after paid never regresses the payment", caseWebhookLatePending},
	{"WH-07", "a second captured payment of the same order_ref is refused or flagged in also_captured", caseDoubleCharge},
	{"WH-08", "a forged notification saying paid for a still-pending session is not believed", caseWebhookForgedPaid},
	// -------------------------------------------------------------- status
	{"ST-01", "status re-read agrees with what was charged (provider is the truth)", caseStatusAgrees},
	{"ST-02", "an unknown reference is an error, never an invented payment", caseStatusUnknown},
	{"ST-03", "failed and expired are reported as final states, valid from pending", caseStatusFinal},
	// -------------------------------------------------------------- refund
	{"RF-01", "partial refund: contract output, exact amount, payment partially_refunded", caseRefundPartial},
	{"RF-02", "refund is idempotent on its key and creates exactly one remote refund", caseRefundIdempotent},
	{"RF-03", "refunding more than the captured amount is rejected", caseRefundOver},
	{"RF-04", "full refund moves the payment to refunded", caseRefundFull},
	{"RF-05", "a payment that was not captured cannot be refunded", caseRefundUncaptured},
	{"RF-06", "pending refunds count against what is left to refund", caseRefundPendingAccumulates},
	{"RF-07", "concurrent refunds never exceed the captured amount", caseRefundConcurrent},
	// -------------------------------------------------------- indeterminate
	{"IND-01", "lost session response is an indeterminate error; the retry leaves exactly one session", caseIndSession},
	{"IND-02", "lost refund response is an indeterminate error; the retry leaves exactly one refund", caseIndRefund},
	{"IND-03", "provider outage on status is an error, never paid / failed / not_found; recovers to paid", caseIndStatusDown},
}

// rejected: the provider answered "no" (typed or plain error), not "I do not
// know" (a timeout says nothing about acceptance).
func rejected(err error) bool { return err != nil && !indeterminate(err) }

// ------------------------------------------------------------------ session

func caseSession(e *env) error {
	s, err := e.session(e.newOrder("149.90", "MXN"))
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
	ef, err := e.effects(o.OrderRef)
	if err != nil {
		return err
	}
	if ef.Sessions != 1 {
		return failf("the provider holds %d sessions for the order after two calls with one key, want 1", ef.Sessions)
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
		c := codeOf(err)
		if !strings.Contains(c, "idempot") && !strings.Contains(c, "conflict") {
			return failf("reusing a key with another amount must fail with a typed idempotency conflict (code like idempotency_conflict), got: %v", err)
		}
		return nil
	}
	if b.SessionID != a.SessionID {
		return failf("same key with another amount created a new session %q", b.SessionID)
	}
	st, err := e.status(map[string]any{"session_id": a.SessionID})
	if err == nil && !sameMoney(st.Amount, "50.00") {
		return failf("reusing the key rewrote the session amount to %s", st.Amount)
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
	// e.session already enforces test_mode=true on every session of the run.
	_, err := e.session(e.newOrder("10.00", "MXN"))
	return err
}

// -------------------------------------------------------------------- money

func caseMoneyNotQuantized(e *env) error {
	for _, c := range [][2]string{{"10.005", "MXN"}, {"10.50", "JPY"}, {"10.0005", "KWD"}} {
		o := e.newOrder(c[0], c[1])
		if _, err := e.callRaw(capability.CommercePaymentsSession, o.sessionInput()); !rejected(err) {
			return failf("%s %s: want a rejection, got %v (more decimals than the currency)", c[0], c[1], err)
		}
	}
	return nil
}

func caseMoneyMalformed(e *env) error {
	for _, a := range []string{"0", "0.00", "-5.00", "1e3", "1,000.00", "10.5.0", ".5", "abc", ""} {
		o := e.newOrder(a, "MXN")
		if _, err := e.callRaw(capability.CommercePaymentsSession, o.sessionInput()); !rejected(err) {
			return failf("amount %q: want a rejection, got %v", a, err)
		}
	}
	return nil
}

// currencyNA handles a provider that cannot charge a currency at all. The
// typed unsupported_currency answer makes the case not_applicable only if the
// provider declared the currency in Target.Supports.UnsupportedCurrencies AND
// refuses it consistently: the well-quantized amounts in probes are refused
// with the same code too. Otherwise the case fails (a currency refusal that
// depends on the amount is a bug, not a limit).
func (e *env) currencyNA(amount, currency string, probes ...string) error {
	_, err := e.session(e.newOrder(amount, currency))
	if codeOf(err) != UnsupportedCurrencyCode {
		return nil // accepted, or refused for another reason: the case judges it
	}
	declared := false
	for _, c := range e.t.Supports.UnsupportedCurrencies {
		declared = declared || c == currency
	}
	if !declared {
		return failf("%s was refused as %s but is not declared in Target.Supports.UnsupportedCurrencies", currency, UnsupportedCurrencyCode)
	}
	for _, p := range append([]string{amount}, probes...) {
		if _, err := e.session(e.newOrder(p, currency)); codeOf(err) != UnsupportedCurrencyCode {
			return failf("%s is declared unsupported but %s %s was not refused as %s (got %v): the refusal must not depend on the amount", currency, p, currency, UnsupportedCurrencyCode, err)
		}
	}
	return naf("the provider declares %s unsupported (%s)", currency, UnsupportedCurrencyCode)
}

func caseMoneyZeroDecimal(e *env) error {
	if err := e.currencyNA("1500", "JPY", "1", "2500"); err != nil {
		return err
	}
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

func checkOutcomeMoney(o payments.Outcome, ord order, want string) error {
	if o.Currency != ord.Currency {
		return failf("currency %q, want %q", o.Currency, ord.Currency)
	}
	if _, err := payments.ParseMoney(o.Amount, o.Currency); err != nil {
		return failf("amount %q is not valid money: %v", o.Amount, err)
	}
	if !sameMoney(o.Amount, want) {
		return failf("amount %q, want %q", o.Amount, want)
	}
	if o.OrderRef != ord.OrderRef {
		return failf("order_ref %q, want %q", o.OrderRef, ord.OrderRef)
	}
	return nil
}

func caseMoneyReportsCharged(e *env) error {
	o := e.newOrder("19.99", "MXN")
	p, err := e.payCharged(o, "19.50")
	if err != nil {
		return err
	}
	if err := checkOutcomeMoney(p.Outcome, o, "19.50"); err != nil {
		return failf("confirm: %v (the provider echoed the session instead of what was charged)", err)
	}
	stored := payments.Payment{Status: payments.StatusPending, Amount: o.Amount, Currency: o.Currency}
	if ob := payments.ObserveOutcome(stored, p.Outcome); ob.Decision != payments.Anomaly || ob.Reason != payments.ReasonAmountMismatch {
		return failf("the ledger must flag the mismatch as anomaly amount_mismatch, got %s %s", ob.Decision, ob.Reason)
	}
	st, err := e.status(map[string]any{"provider_ref": p.Outcome.ProviderRef})
	if err != nil {
		return err
	}
	return checkOutcomeMoney(st, o, "19.50")
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
		if r.Status == "pending" {
			if err := e.eventually(func() error { return refundSettled(e, p.Outcome.ProviderRef) }); err != nil {
				return skipf("refund still pending: %v", err)
			}
		}
	}
	return e.eventually(func() error {
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
	})
}

func refundSettled(e *env, ref string) error {
	o, err := e.status(map[string]any{"provider_ref": ref})
	if err != nil {
		return err
	}
	if o.AmountRefunded == "" {
		return failf("no amount_refunded yet")
	}
	return nil
}

func caseMoneyThreeDecimals(e *env) error {
	if err := e.currencyNA("10.005", "KWD", "10.000", "10.00", "1.500"); err != nil {
		return err
	}
	p, err := e.pay("10.005", "KWD")
	if err != nil {
		return failf("KWD has 3 decimals, 10.005 is valid money: %v", err)
	}
	if !sameMoney(p.Outcome.Amount, "10.005") || p.Outcome.Currency != "KWD" {
		return failf("KWD 10.005 came back as %s %s", p.Outcome.Currency, p.Outcome.Amount)
	}
	o := e.newOrder("10.0005", "KWD")
	if _, err := e.callRaw(capability.CommercePaymentsSession, o.sessionInput()); !rejected(err) {
		return failf("KWD 10.0005 must be rejected, got %v", err)
	}
	return nil
}

func caseMoneyBig(e *env) error {
	for _, amt := range []string{"9007199254740993.01", "99999999999999999999.99"} {
		o := e.newOrder(amt, "MXN")
		s, err := e.session(o)
		if err != nil {
			if rejected(err) {
				continue // a documented ceiling is fine; silently rounding is not
			}
			return err
		}
		w, err := e.t.Sandbox.Settle(e.ctx, SettleSpec{SessionID: s.SessionID, Status: payments.StatusPaid})
		if err != nil {
			return failf("sandbox settle: %v", err)
		}
		out, err := e.confirm(w)
		if err != nil {
			return failf("confirm: %v", err)
		}
		if !sameMoney(out.Amount, amt) {
			return failf("%s MXN came back as %s: precision lost (float64 / int64 somewhere)", amt, out.Amount)
		}
	}
	return nil
}

func caseMoneyBadCurrency(e *env) error {
	for _, c := range []string{"mxn", "Mxn", "MX", "MEXICO", "M1N", ""} {
		o := e.newOrder("10.00", c)
		if _, err := e.callRaw(capability.CommercePaymentsSession, o.sessionInput()); !rejected(err) {
			return failf("currency %q: want a rejection, got %v", c, err)
		}
	}
	return nil
}

func caseMoneyRefundCurrency(e *env) error {
	p, err := e.pay("10.00", "MXN")
	if err != nil {
		return err
	}
	if _, err := e.refund(p.Outcome.ProviderRef, "1.00", "USD", e.next("rkey")); !rejected(err) {
		return failf("a USD refund of a MXN payment: want a rejection, got %v", err)
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
	if err := checkOutcomeMoney(o, p.Order, p.Order.Amount); err != nil {
		return err
	}
	stored := payments.Payment{Status: payments.StatusPending, Amount: p.Order.Amount, Currency: p.Order.Currency}
	if ob := payments.ObserveOutcome(stored, o); ob.Decision != payments.Apply {
		return failf("the ledger would not apply the confirmation: %s %s", ob.Decision, ob.Reason)
	}
	return nil
}

// corruptSignature returns the headers with the signature corrupted: the
// longest header value (a signature is the longest, e.g. a hex HMAC) gets its
// last character changed. The body stays genuine.
func corruptSignature(h map[string]string) map[string]string {
	out := map[string]string{}
	longest := ""
	for k, v := range h {
		out[k] = v
		if len(v) > len(h[longest]) {
			longest = k
		}
	}
	if v := out[longest]; v != "" {
		last := v[len(v)-1]
		repl := byte('0')
		if last == '0' {
			repl = '1'
		}
		out[longest] = v[:len(v)-1] + string(repl)
	}
	return out
}

// tamperBody alters the fields that have effect (any "amount*" and "status"
// key, at any depth) so the notification claims something else, keeping every
// id intact: the provider cannot reject it merely because a lookup failed.
// A body that is not JSON gets its digits shifted.
func tamperBody(body string) string {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return shiftDigits(body)
	}
	changed := false
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				lk := strings.ToLower(k)
				switch {
				case strings.Contains(lk, "amount"):
					if s, ok := val.(string); ok {
						t[k] = "999999.00"
						_ = s
						changed = true
					} else if _, ok := val.(float64); ok {
						t[k] = 999999.0
						changed = true
					}
				case lk == "status" || strings.HasSuffix(lk, "_status"):
					if s, ok := val.(string); ok {
						if strings.EqualFold(s, "paid") {
							t[k] = "failed"
						} else {
							t[k] = "paid"
						}
						changed = true
					}
				default:
					t[k] = walk(val)
				}
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return x
	}
	v = walk(v)
	if !changed {
		return shiftDigits(body)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func shiftDigits(body string) string {
	b := []byte(body)
	changed := false
	for i, c := range b {
		if c >= '0' && c <= '9' {
			b[i] = '0' + (c-'0'+1)%10
			changed = true
		}
	}
	if !changed {
		return body + " "
	}
	return string(b)
}

func sameOutcome(a, b payments.Outcome) bool {
	return a.Status == b.Status && a.Currency == b.Currency && a.ProviderRef == b.ProviderRef &&
		a.OrderRef == b.OrderRef && sameMoney(a.Amount, b.Amount)
}

// untrusted sends a notification that must not be believed: signed mode
// rejects it; hint+reread mode may answer, but only the genuine payment.
func (e *env) untrusted(what string, genuine payments.Outcome, w Webhook) error {
	o, err := e.confirm(w)
	if err != nil {
		return nil
	}
	if e.opts.WebhookMode == WebhookHintReread && sameOutcome(o, genuine) {
		return nil
	}
	return failf("%s was accepted as %s %s %s", what, o.Status, o.Amount, o.Currency)
}

func (e *env) genuine() (paid, error) {
	s, err := e.session(e.newOrder("100.00", "MXN"))
	if err != nil {
		return paid{}, err
	}
	p := paid{Session: s}
	if p.Webhook, err = e.t.Sandbox.Settle(e.ctx, SettleSpec{SessionID: s.SessionID, Status: payments.StatusPaid}); err != nil {
		return p, failf("sandbox settle: %v", err)
	}
	if p.Outcome, err = e.confirm(p.Webhook); err != nil {
		return p, failf("confirm of the genuine webhook: %v", err)
	}
	return p, nil
}

func caseWebhookTampered(e *env) error {
	p, err := e.genuine()
	if err != nil {
		return err
	}
	// 1. the genuine body with a corrupted signature: only a provider that
	// really verifies the HMAC rejects it (the lookup would succeed).
	bad := Webhook{RawBody: p.Webhook.RawBody, Query: p.Webhook.Query, Headers: corruptSignature(p.Webhook.Headers)}
	if err := e.untrusted("a genuine webhook with a corrupted signature", p.Outcome, bad); err != nil {
		return err
	}
	// 2. altered amount / status under the original signature.
	w := p.Webhook
	w.RawBody = tamperBody(w.RawBody)
	if w.RawBody == p.Webhook.RawBody {
		return failf("suite could not alter the body")
	}
	return e.untrusted("a webhook with altered amount / status under the original signature", p.Outcome, w)
}

func caseWebhookUnsigned(e *env) error {
	p, err := e.genuine()
	if err != nil {
		return err
	}
	w := Webhook{RawBody: p.Webhook.RawBody, Query: p.Webhook.Query} // headers gone, query kept
	return e.untrusted("a webhook with no signature headers", p.Outcome, w)
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

func caseWebhookStale(e *env) error {
	p, err := e.genuine()
	if err != nil {
		return err
	}
	old, err := e.t.Sandbox.Age(e.ctx, p.Webhook, time.Hour)
	if err != nil {
		if errorsIsUnsupported(err) {
			return skipf("sandbox cannot re-issue a webhook with an old timestamp")
		}
		return failf("sandbox age: %v", err)
	}
	return e.untrusted("a validly signed webhook one hour old", p.Outcome, old)
}

func caseWebhookLatePending(e *env) error {
	p, err := e.pay("55.00", "MXN")
	if err != nil {
		return err
	}
	w, err := e.t.Sandbox.Settle(e.ctx, SettleSpec{SessionID: p.Session.SessionID, Status: payments.StatusPending})
	if err != nil {
		if errorsIsUnsupported(err) {
			return skipf("sandbox cannot emit a late pending notification")
		}
		return failf("sandbox settle pending: %v", err)
	}
	if o, err := e.confirm(w); err == nil {
		stored := payments.Payment{Status: payments.StatusPaid, Amount: p.Order.Amount, Currency: p.Order.Currency, ProviderRef: p.Outcome.ProviderRef}
		if ob := payments.ObserveOutcome(stored, o); ob.Decision != payments.Ignore {
			return failf("a late %s after paid: the ledger would %s (%s)", o.Status, ob.Decision, ob.Reason)
		}
	}
	st, err := e.status(map[string]any{"provider_ref": p.Outcome.ProviderRef})
	if err != nil {
		return err
	}
	if st.Status != payments.StatusPaid {
		return failf("after a late pending notification the payment reads %s, want paid", st.Status)
	}
	return nil
}

// caseWebhookForgedPaid builds an unsigned notification that says paid for a
// session that is not paid: from the real paid notification of session A it
// points the ids at session B (still pending) or, when the provider's body
// carries no id of A that B could replace (no reference before the buyer pays),
// at a reference that does not exist. It must be rejected, or answered with the
// genuine state: nothing may end up captured because of it.
func caseWebhookForgedPaid(e *env) error {
	a, err := e.session(e.newOrder("90.00", "MXN"))
	if err != nil {
		return err
	}
	bo := e.newOrder("90.00", "MXN")
	b, err := e.session(bo)
	if err != nil {
		return err
	}
	w, err := e.t.Sandbox.Settle(e.ctx, SettleSpec{SessionID: a.SessionID, Status: payments.StatusPaid})
	if err != nil {
		return failf("sandbox settle: %v", err)
	}
	body := w.RawBody
	replace := func(from, to string) {
		if from != "" && to != "" {
			body = strings.ReplaceAll(body, from, to)
		}
	}
	replace(a.SessionID, b.SessionID)
	replace(orderOf(e, a), bo.OrderRef)
	replace(a.ProviderRef, b.ProviderRef)
	if body == w.RawBody {
		// nothing of A to repoint at B: forge every numeric reference
		body = digitRuns.ReplaceAllString(w.RawBody, "900000000017")
	}
	if body == w.RawBody {
		body += " "
	}
	forged := Webhook{RawBody: body, Query: w.Query} // no signature
	if o, err := e.confirm(forged); err == nil && o.Status.Captured() {
		return failf("an unsigned notification for a payment that was not made came back %s (%s)", o.Status, o.ProviderRef)
	}
	st, err := e.status(map[string]any{"session_id": b.SessionID})
	if err == nil && st.Status.Captured() {
		return failf("after a forged notification the pending session reads %s", st.Status)
	}
	return nil
}

var digitRuns = regexp.MustCompile(`[0-9]+`)

// orderOf is the order_ref of session s as the provider reports it.
func orderOf(e *env, s sessionOut) string {
	if st, err := e.status(map[string]any{"session_id": s.SessionID}); err == nil {
		return st.OrderRef
	}
	return "\x00none"
}

func caseDoubleCharge(e *env) error {
	p, err := e.pay("70.00", "MXN")
	if err != nil {
		return err
	}
	second := order{OrderRef: p.Order.OrderRef, Key: e.next("key"), Amount: p.Order.Amount, Currency: p.Order.Currency}
	s, err := e.session(second)
	if err != nil {
		if rejected(err) {
			return nil // refusing a second session of a paid order is the best answer
		}
		return err
	}
	w, err := e.t.Sandbox.Settle(e.ctx, SettleSpec{SessionID: s.SessionID, Status: payments.StatusPaid})
	if err != nil {
		return failf("sandbox settle: %v", err)
	}
	x, err := e.confirmX(w)
	if err != nil {
		return nil // refusing to confirm the duplicate is also an answer
	}
	for _, r := range x.AlsoCaptured {
		if r == p.Outcome.ProviderRef {
			return nil
		}
	}
	return failf("a second payment of the same order was captured and not flagged: also_captured=%v, want %q", x.AlsoCaptured, p.Outcome.ProviderRef)
}

// ------------------------------------------------------------------- status

func caseStatusAgrees(e *env) error {
	o := e.newOrder("42.00", "MXN")
	p, err := e.payCharged(o, "41.00")
	if err != nil {
		return err
	}
	for _, in := range []map[string]any{
		{"provider_ref": p.Outcome.ProviderRef},
		{"order_ref": o.OrderRef},
	} {
		st, err := e.status(in)
		if err != nil {
			return failf("status %v: %v", in, err)
		}
		if st.Status != payments.StatusPaid {
			return failf("status %v: %s, webhook said paid", in, st.Status)
		}
		if err := checkOutcomeMoney(st, o, "41.00"); err != nil {
			return failf("status %v: %v", in, err)
		}
		if st.ProviderRef != p.Outcome.ProviderRef {
			return failf("status %v answers another payment: %q vs %q", in, st.ProviderRef, p.Outcome.ProviderRef)
		}
	}
	return nil
}

func caseStatusUnknown(e *env) error {
	o, err := e.status(map[string]any{"provider_ref": e.next("nope")})
	if err == nil {
		return failf("an unknown provider_ref answered %s", o.Status)
	}
	if indeterminate(err) {
		return failf("an unknown reference must be rejected, got an indeterminate error: %v", err)
	}
	return nil
}

func caseStatusFinal(e *env) error {
	for _, st := range []payments.Status{payments.StatusFailed, payments.StatusExpired} {
		s, err := e.session(e.newOrder("30.00", "MXN"))
		if err != nil {
			return err
		}
		w, err := e.t.Sandbox.Settle(e.ctx, SettleSpec{SessionID: s.SessionID, Status: st})
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

// awaitRefund resolves a refund answer into a verdict: the payment must end
// reflecting it. A refund that stays pending is inconclusive (skip, which
// fails a strict run), never a pass.
func (e *env) awaitRefund(r refundOut, ref, want string, st payments.Status) error {
	switch r.Status {
	case "failed":
		return failf("the refund failed")
	case "manual_required":
		return skipf("refund needs manual settlement")
	}
	err := e.eventually(func() error { return refundedOK(e, ref, want, st) })
	if err != nil && r.Status == "pending" {
		return skipf("refund still pending after %s: %v", e.opts.PollTimeout, err)
	}
	return err
}

func caseRefundPartial(e *env) error {
	p, err := e.pay("20.00", "MXN")
	if err != nil {
		return err
	}
	ref := p.Outcome.ProviderRef
	r, err := e.refund(ref, "5.00", "MXN", e.next("rkey"))
	if err != nil {
		return err
	}
	if r.RefundID == "" || !sameMoney(r.Amount, "5.00") || r.Currency != "MXN" {
		return failf("refund answered id %q %s %s", r.RefundID, r.Amount, r.Currency)
	}
	if !validRefundStatus(r.Status) {
		return failf("refund status %q", r.Status)
	}
	if err := e.awaitRefund(r, ref, "5.00", payments.StatusPartiallyRefunded); err != nil {
		return err
	}
	o, _ := e.status(map[string]any{"provider_ref": ref})
	stored := payments.Payment{Status: payments.StatusPaid, Amount: "20.00", Currency: "MXN", ProviderRef: ref}
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
	ref := p.Outcome.ProviderRef
	key := e.next("rkey")
	a, err := e.refund(ref, "5.00", "MXN", key)
	if err != nil {
		return err
	}
	b, err := e.refund(ref, "5.00", "MXN", key)
	if err != nil {
		return failf("retry with the same key failed: %v", err)
	}
	if a.RefundID != b.RefundID {
		return failf("same key created two refunds: %q and %q", a.RefundID, b.RefundID)
	}
	// the same key with another amount must not refund again either
	if c, err := e.refund(ref, "7.00", "MXN", key); err == nil && c.RefundID != a.RefundID {
		return failf("same key with another amount created refund %q", c.RefundID)
	}
	if err := e.awaitRefund(a, ref, "5.00", payments.StatusPartiallyRefunded); err != nil {
		return err
	}
	ef, err := e.effects(p.Order.OrderRef)
	if err != nil {
		return err
	}
	if ef.Refunds != 1 {
		return failf("the provider holds %d refunds after retries with one key, want 1", ef.Refunds)
	}
	return nil
}

func caseRefundOver(e *env) error {
	p, err := e.pay("10.00", "MXN")
	if err != nil {
		return err
	}
	ref := p.Outcome.ProviderRef
	if _, err := e.refund(ref, "10.01", "MXN", e.next("rkey")); !rejected(err) {
		return failf("refund of 10.01 on a payment of 10.00: want a rejection, got %v", err)
	}
	r, err := e.refund(ref, "10.00", "MXN", e.next("rkey"))
	if err != nil {
		return failf("full refund after a rejected over-refund: %v", err)
	}
	if err := e.awaitRefund(r, ref, "10.00", payments.StatusRefunded); err != nil {
		return err
	}
	if _, err := e.refund(ref, "0.01", "MXN", e.next("rkey")); !rejected(err) {
		return failf("a refund on a fully refunded payment: want a rejection, got %v", err)
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
	return e.awaitRefund(r, p.Outcome.ProviderRef, "12.34", payments.StatusRefunded)
}

func caseRefundUncaptured(e *env) error {
	s, err := e.session(e.newOrder("10.00", "MXN"))
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
		if r := e.t.Supports.NoRefBeforePay; r != "" {
			return naf("%s", r)
		}
		return skipf("the provider holds no payment reference before the buyer pays (declare Target.Supports.NoRefBeforePay if by design)")
	}
	if _, err := e.refund(ref, "1.00", "MXN", e.next("rkey")); !rejected(err) {
		return failf("a payment still pending: want the refund rejected, got %v", err)
	}
	return nil
}

func caseRefundPendingAccumulates(e *env) error {
	p, err := e.pay("10.00", "MXN")
	if err != nil {
		return err
	}
	ref := p.Outcome.ProviderRef
	r, err := e.refund(ref, "6.00", "MXN", e.next("rkey"))
	if err != nil {
		return err
	}
	if r.Status == "failed" {
		return failf("the first refund failed")
	}
	// whether the first refund is pending or succeeded, 6.00 + 6.00 > 10.00
	if r2, err := e.refund(ref, "6.00", "MXN", e.next("rkey")); err == nil {
		return failf("a second refund of 6.00 on 10.00 was accepted (%s): pending refunds do not count against the remainder", r2.Status)
	} else if indeterminate(err) {
		return failf("want a rejection, got: %v", err)
	}
	return nil
}

func caseRefundConcurrent(e *env) error {
	p, err := e.pay("10.00", "MXN")
	if err != nil {
		return err
	}
	ref := p.Outcome.ProviderRef
	const n = 6
	keys := make([]string, n)
	for i := range keys {
		keys[i] = e.next("rkey")
	}
	var wg sync.WaitGroup
	okc := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.refund(ref, "4.00", "MXN", keys[i])
			okc[i] = err == nil && r.Status != "failed"
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, b := range okc {
		if b {
			ok++
		}
	}
	if ok > 2 {
		return failf("%d concurrent refunds of 4.00 were accepted on a payment of 10.00", ok)
	}
	return e.eventually(func() error {
		o, err := e.status(map[string]any{"provider_ref": ref})
		if err != nil {
			return err
		}
		got, err := payments.ParseAmount(o.AmountRefunded)
		if o.AmountRefunded != "" && err != nil {
			return failf("amount_refunded %q", o.AmountRefunded)
		}
		if got.Cmp(payments.MustAmount("10.00")) > 0 {
			return failf("amount_refunded %s exceeds the captured 10.00", o.AmountRefunded)
		}
		return nil
	})
}

// ------------------------------------------------------------ indeterminate

func caseIndSession(e *env) error {
	o := e.newOrder("60.00", "MXN")
	if _, err := e.inject(FaultSessionLostResponse); err != nil {
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
	ef, err := e.effects(o.OrderRef)
	if err != nil {
		return err
	}
	if ef.Sessions != 1 {
		return failf("after the lost response and the retry the provider holds %d sessions for the order, want exactly 1 (a double charge risk)", ef.Sessions)
	}
	return nil
}

func caseIndRefund(e *env) error {
	p, err := e.pay("20.00", "MXN")
	if err != nil {
		return err
	}
	ref := p.Outcome.ProviderRef
	key := e.next("rkey")
	if _, err := e.inject(FaultRefundLostResponse); err != nil {
		return err
	}
	if _, err := e.refund(ref, "5.00", "MXN", key); err == nil {
		return failf("a lost refund response was masked as an answer")
	} else if !indeterminate(err) {
		return failf("a lost refund response must be indeterminate, got: %v", err)
	}
	r, err := e.refund(ref, "5.00", "MXN", key)
	if err != nil {
		return failf("retry with the same key: %v", err)
	}
	if err := e.awaitRefund(r, ref, "5.00", payments.StatusPartiallyRefunded); err != nil { // once, not twice
		return err
	}
	ef, err := e.effects(p.Order.OrderRef)
	if err != nil {
		return err
	}
	if ef.Refunds != 1 {
		return failf("after the lost response and the retry the provider holds %d refunds, want exactly 1", ef.Refunds)
	}
	return nil
}

func caseIndStatusDown(e *env) error {
	p, err := e.pay("20.00", "MXN")
	if err != nil {
		return err
	}
	restore, err := e.inject(FaultProviderDown)
	if err != nil {
		return err
	}
	o, err := e.status(map[string]any{"provider_ref": p.Outcome.ProviderRef})
	if err == nil {
		return failf("status during an outage answered %s: an unreachable provider is not an answer", o.Status)
	}
	if codeOf(err) == capability.PaymentsStatusNotFound {
		return failf("an outage was reported as not_found, which is authoritative: the consumer may release the attempt")
	}
	restore()
	return e.eventually(func() error {
		o, err := e.status(map[string]any{"provider_ref": p.Outcome.ProviderRef})
		if err != nil {
			return failf("after the outage: %v", err)
		}
		if o.Status != payments.StatusPaid {
			return failf("after the outage the payment reads %s, want paid", o.Status)
		}
		return nil
	})
}
