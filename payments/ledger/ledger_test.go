package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"gorm.io/gorm"
)

var ctx = context.Background()

func cashCharge(key, amount string) ChargeRequest {
	return ChargeRequest{
		DocumentType: "sales_order", DocumentID: "SO-1", DocumentTotal: "100.00", Amount: amount, Currency: "MXN",
		Channel: payments.ChannelCash, Provider: payments.ProviderCash, IdempotencyKey: key,
		Cash: &payments.CashTender{Tendered: "200", Change: "100"}, Caller: "pos",
	}
}

func terminalCharge(key string) ChargeRequest {
	return ChargeRequest{
		DocumentType: "sales_order", DocumentID: "SO-1", DocumentTotal: "100.00", Amount: "100.00", Currency: "MXN",
		Channel: payments.ChannelTerminal, Provider: "mercadopago", DeviceID: "PAX", IdempotencyKey: key, Caller: "pos",
	}
}

func TestHolds(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		req := payments.HoldRequest{DocumentType: "sales_order", DocumentID: "SO-1", Holder: "kiosk:K1", Channel: payments.ChannelTerminal}
		got, err := h.l.AcquireHold(ctx, h.org, req, "kiosk")
		if err != nil || got.Token == "" {
			t.Fatalf("acquire: %+v %v", got, err)
		}
		// The token never lands in the database.
		var n int64
		db.Model(&holdRow{}).Where("token_hash = ?", got.Token).Count(&n)
		if n != 0 {
			t.Fatal("raw token stored")
		}
		// 7leguas #604: the cash desk cannot hold (or charge) the ticket the
		// kiosk's recycler is taking bills for.
		other := payments.HoldRequest{DocumentType: "sales_order", DocumentID: "SO-1", Holder: "pos:caja-1"}
		if _, err := h.l.AcquireHold(ctx, h.org, other, "pos"); ErrorCode(err) != CodeInPayment {
			t.Fatalf("second holder: %v", err)
		}
		if _, err := h.l.StartCharge(ctx, h.org, cashCharge("k1", "100.00")); ErrorCode(err) != CodeInPayment {
			t.Fatalf("charge without token: %v", err)
		}
		// Owner renews with its token.
		req.Token = got.Token
		h.advance(4 * time.Minute)
		renewed, err := h.l.AcquireHold(ctx, h.org, req, "kiosk")
		if err != nil || renewed.Token != got.Token || !renewed.Hold.ExpiresAt.Equal(h.now.Add(payments.DefaultHoldTTL)) {
			t.Fatalf("renew: %+v %v", renewed, err)
		}
		if err := h.l.ReleaseHold(ctx, h.org, "sales_order", "SO-1", "wrong", false); ErrorCode(err) != CodeHoldNotOwner {
			t.Fatalf("release by non-owner: %v", err)
		}
		// A kiosk that died: after expiry anyone takes it.
		h.advance(payments.DefaultHoldTTL + time.Second)
		taken, err := h.l.AcquireHold(ctx, h.org, other, "pos")
		if err != nil || taken.Token == got.Token {
			t.Fatalf("takeover: %+v %v", taken, err)
		}
		doc, err := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if err != nil || doc.Hold == nil || doc.Hold.Holder != "pos:caja-1" || doc.Hold.Token != "" {
			t.Fatalf("document hold: %+v %v", doc.Hold, err)
		}
		if err := h.l.ReleaseHold(ctx, h.org, "sales_order", "SO-1", taken.Token, false); err != nil {
			t.Fatal(err)
		}
		if doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1"); doc.Hold != nil {
			t.Fatal("released hold still live")
		}
	})
}

func TestCashChargeReplayAndHoldConsumed(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		hold, err := h.l.AcquireHold(ctx, h.org, payments.HoldRequest{DocumentType: "sales_order", DocumentID: "SO-1", Holder: "pos:1"}, "pos")
		if err != nil {
			t.Fatal(err)
		}
		req := cashCharge("req-1", "100.00")
		req.HoldToken = hold.Token
		res, err := h.l.StartCharge(ctx, h.org, req)
		if err != nil || res.Verdict != payments.VerdictNew || res.Payment.Status != payments.StatusPaid || res.Payment.Cash == nil {
			t.Fatalf("cash: %+v %v", res, err)
		}
		ev := h.confirmed()
		if len(ev) != 1 || ev[0].emitter != payments.ProviderCash || ev[0].payload["payment_id"] != res.Payment.ID {
			t.Fatalf("published %+v", ev)
		}
		// Timeout retry with the same key: replay, nothing new (7leguas 1eb71a72e).
		again, err := h.l.StartCharge(ctx, h.org, req)
		if err != nil || again.Verdict != payments.VerdictReplay || again.Payment.ID != res.Payment.ID {
			t.Fatalf("replay: %+v %v", again, err)
		}
		// Another point of sale: already paid, nothing charged.
		other := cashCharge("req-2", "100.00")
		if r, err := h.l.StartCharge(ctx, h.org, other); err != nil || r.Verdict != payments.VerdictAlreadyPaid {
			t.Fatalf("already paid: %+v %v", r, err)
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if len(doc.Payments) != 1 || doc.NetPaid != "100.00" || doc.Hold != nil {
			t.Fatalf("document: %+v", doc)
		}
		if len(h.confirmed()) != 1 {
			t.Fatal("replays must not publish")
		}
	})
}

func TestChargeValidationAndDue(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		bad := cashCharge("k", "100")
		bad.Amount = "100.5e0"
		if _, err := h.l.StartCharge(ctx, h.org, bad); ErrorCode(err) != CodeInvalidInput {
			t.Fatalf("float amount: %v", err)
		}
		bad = cashCharge("k", "100")
		bad.Provider = "mercadopago"
		if _, err := h.l.StartCharge(ctx, h.org, bad); ErrorCode(err) != CodeInvalidInput {
			t.Fatalf("cash with provider: %v", err)
		}
		if _, err := h.l.StartCharge(ctx, h.org, cashCharge("k1", "60")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.l.StartCharge(ctx, h.org, cashCharge("k2", "40.01")); ErrorCode(err) != CodeExceedsDue {
			t.Fatalf("over the due amount: %v", err)
		}
		if r, err := h.l.StartCharge(ctx, h.org, cashCharge("k3", "40")); err != nil || r.Verdict != payments.VerdictNew {
			t.Fatalf("second tender: %+v %v", r, err)
		}
	})
}

func TestTerminalChargeFlow(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(in map[string]any) (any, error) {
			if in["device_id"] != "PAX" || in["amount"] != "100.00" || in["idempotency_key"] != "t-1" {
				t.Errorf("terminal input %v", in)
			}
			return map[string]any{"provider_ref": "ORD01", "status": "at_device", "expires_at": h.now.Add(10 * time.Minute).Format(time.RFC3339)}, nil
		})
		status := "at_device"
		h.fake.on("mercadopago", capability.CommercePaymentsStatus, func(in map[string]any) (any, error) {
			if in["provider_ref"] != "ORD01" {
				t.Errorf("status input %v", in)
			}
			s := statusOf("ORD01", status, "100.00")
			s["card"] = map[string]any{"type": "debit", "brand": "debmaster", "last4": "0042"}
			s["channel"] = "terminal"
			return s, nil
		})
		res, err := h.l.StartCharge(ctx, h.org, terminalCharge("t-1"))
		if err != nil || res.Payment.Status != payments.StatusAtDevice || res.Payment.ProviderRef != "ORD01" {
			t.Fatalf("start: %+v %v", res, err)
		}
		// Same request again (screen reload): resume, never a second order.
		again, err := h.l.StartCharge(ctx, h.org, terminalCharge("t-1"))
		if err != nil || again.Verdict != payments.VerdictInProgress || again.Payment.ID != res.Payment.ID || h.fake.count(capability.CommercePaymentsTerminalCreate) != 1 {
			t.Fatalf("resume: %+v %v", again, err)
		}
		// Another attempt while it is on the device: in progress too.
		if r, _ := h.l.StartCharge(ctx, h.org, terminalCharge("t-2")); r.Verdict != payments.VerdictInProgress {
			t.Fatalf("parallel attempt: %s", r.Verdict)
		}
		status = "paid"
		obs, err := h.l.Refresh(ctx, h.org, res.Payment.ID, payments.SourcePoll)
		if err != nil || obs.Decision != payments.Apply || obs.Payment.Status != payments.StatusPaid || obs.Payment.Card == nil || obs.Payment.Card.Last4 != "0042" {
			t.Fatalf("paid: %+v %v", obs, err)
		}
		// at_device (the customer display shows "pay on the terminal") and paid.
		ev := h.confirmed()
		if len(ev) != 2 || ev[1].emitter != "mercadopago" || ev[1].payload["document_id"] != "SO-1" ||
			ev[1].payload["result"].(map[string]any)["status"] != "paid" {
			t.Fatalf("published %+v", ev)
		}
		// Re-polling the same state is a duplicate: no further event.
		obs, err = h.l.Refresh(ctx, h.org, res.Payment.ID, payments.SourcePoll)
		if err != nil || !obs.Duplicate || len(h.confirmed()) != 2 {
			t.Fatalf("duplicate: %+v %v", obs, err)
		}
	})
}

func TestTerminalRefusedAndIndeterminate(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(in map[string]any) (any, error) {
			if in["idempotency_key"] == "slow" {
				return nil, timeoutErr()
			}
			return nil, capability.NewCodedError("amount_below_minimum", "minimum is 5.00")
		})
		res, err := h.l.StartCharge(ctx, h.org, terminalCharge("low"))
		if ErrorCode(err) != "amount_below_minimum" || res.Payment.Status != payments.StatusFailed {
			t.Fatalf("refused: %+v %v", res.Payment, err)
		}
		res, err = h.l.StartCharge(ctx, h.org, terminalCharge("slow"))
		if err != nil || !res.Indeterminate || res.Payment.Status != payments.StatusPending {
			t.Fatalf("indeterminate: %+v %v", res, err)
		}
	})
}

func TestCancelLostToTheBuyer(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(map[string]any) (any, error) {
			return map[string]any{"provider_ref": "ORD02", "status": "at_device"}, nil
		})
		// MP Point answers 202: cancellation queued, then the buyer pays.
		h.fake.on("mercadopago", capability.CommercePaymentsCancel, func(map[string]any) (any, error) {
			return map[string]any{"status": "cancelling"}, nil
		})
		h.fake.on("mercadopago", capability.CommercePaymentsStatus, func(map[string]any) (any, error) {
			return statusOf("ORD02", "paid", "100.00"), nil
		})
		res, err := h.l.StartCharge(ctx, h.org, terminalCharge("c-1"))
		if err != nil {
			t.Fatal(err)
		}
		obs, err := h.l.Cancel(ctx, h.org, res.Payment.ID, "cancel-1", "cliente se fue")
		if err != nil || obs.Payment.Status != payments.StatusPaid || obs.Decision != payments.Apply {
			t.Fatalf("cancel race: %+v %v", obs, err)
		}
		if _, err := h.l.Cancel(ctx, h.org, res.Payment.ID, "cancel-2", ""); ErrorCode(err) != CodeInvalidTransition {
			t.Fatalf("cancel a paid payment: %v", err)
		}
	})
}

func TestObserveAuthorizationAnomaliesAndCorrection(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(map[string]any) (any, error) {
			return map[string]any{"provider_ref": "ORD03", "status": "at_device"}, nil
		})
		res, err := h.l.StartCharge(ctx, h.org, terminalCharge("a-1"))
		if err != nil {
			t.Fatal(err)
		}
		paid := payments.Outcome{OrderRef: "SO-1", Status: payments.StatusPaid, Amount: "100.00", Currency: "MXN", ProviderRef: "ORD03", EventID: "evt-1"}
		// Only the provider itself may write about its payments.
		if _, err := h.l.Observe(ctx, h.org, "clip", "mercadopago", paid, payments.SourceWebhook); ErrorCode(err) != CodeForbidden {
			t.Fatalf("another provider: %v", err)
		}
		if _, err := h.l.Observe(ctx, h.org, "pos", "", paid, payments.SourceWebhook); ErrorCode(err) != CodeForbidden {
			t.Fatalf("a sales channel: %v", err)
		}
		bad := paid
		bad.Status = "approved"
		if _, err := h.l.Observe(ctx, h.org, "mercadopago", "", bad, payments.SourceWebhook); ErrorCode(err) != CodeInvalidInput {
			t.Fatalf("raw status: %v", err)
		}
		// Amount mismatch is never applied.
		wrong := paid
		wrong.Amount, wrong.EventID = "10.00", "evt-0"
		obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", wrong, payments.SourceWebhook)
		if err != nil || obs.Decision != payments.Anomaly || obs.Reason != ReasonAmountMismatch || obs.Payment.Status != payments.StatusAtDevice {
			t.Fatalf("mismatch: %+v %v", obs, err)
		}
		// Expired, then a late capture: flagged, not applied...
		exp := paid
		exp.Status, exp.EventID = payments.StatusExpired, "evt-2"
		if obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", exp, payments.SourceWebhook); err != nil || obs.Payment.Status != payments.StatusExpired {
			t.Fatalf("expire: %+v %v", obs, err)
		}
		late := paid
		late.EventID = "evt-3"
		obs, err = h.l.Observe(ctx, h.org, "mercadopago", "", late, payments.SourceWebhook)
		if err != nil || obs.Decision != payments.Anomaly || obs.Reason != payments.ReasonLateCapture || obs.Payment.Status != payments.StatusExpired {
			t.Fatalf("late capture: %+v %v", obs, err)
		}
		// ...until a second consistent read (reconciliation) confirms it.
		h.fake.on("mercadopago", capability.CommercePaymentsStatus, func(map[string]any) (any, error) {
			return statusOf("ORD03", "paid", "100.00"), nil
		})
		obs, err = h.l.Refresh(ctx, h.org, res.Payment.ID, payments.SourceReconcile)
		if err != nil || !obs.Corrected || obs.Payment.Status != payments.StatusPaid || obs.Payment.Anomaly != "" {
			t.Fatalf("correction: %+v %v", obs, err)
		}
		reasons := map[string]bool{}
		for _, a := range h.anomalies {
			reasons[a.Reason] = true
		}
		if !reasons[ReasonAmountMismatch] || !reasons[payments.ReasonLateCapture] {
			t.Fatalf("anomalies %+v", h.anomalies)
		}
		var kernelEvents int
		for _, e := range h.events {
			if e.event == AnomalyEvent && e.emitter == "kernel" {
				kernelEvents++
			}
		}
		if kernelEvents < 2 {
			t.Fatalf("anomaly events %d", kernelEvents)
		}
		// Redelivered webhook: duplicate.
		if obs, _ := h.l.Observe(ctx, h.org, "mercadopago", "", late, payments.SourceWebhook); !obs.Duplicate {
			t.Fatalf("redelivery: %+v", obs)
		}
	})
}

func TestOrphan(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		o := payments.Outcome{OrderRef: "X-9", Status: payments.StatusPaid, Amount: "50", Currency: "MXN", ProviderRef: "unknown"}
		obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourceWebhook)
		if err != nil || !obs.Orphan || len(h.anomalies) != 1 || h.anomalies[0].Reason != ReasonOrphan {
			t.Fatalf("orphan: %+v %v %+v", obs, err, h.anomalies)
		}
		if obs, _ := h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourceReconcile); !obs.Duplicate || len(h.anomalies) != 1 {
			t.Fatalf("orphan twice: %+v", obs)
		}
	})
}

func paidTerminal(t *testing.T, h *harness) payments.Payment {
	t.Helper()
	h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(map[string]any) (any, error) {
		return map[string]any{"provider_ref": "ORD10", "status": "paid"}, nil
	})
	res, err := h.l.StartCharge(ctx, h.org, terminalCharge("p-1"))
	if err != nil || res.Payment.Status != payments.StatusPaid {
		t.Fatalf("paid terminal: %+v %v", res, err)
	}
	return res.Payment
}

func TestRefunds(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(in map[string]any) (any, error) {
			if in["provider_ref"] != "ORD10" {
				t.Errorf("refund input %v", in)
			}
			st := "succeeded"
			if in["idempotency_key"] == "ret-pending" {
				st = "pending"
			}
			return map[string]any{"refund_id": "rf-" + in["idempotency_key"].(string), "status": st, "amount": in["amount"], "currency": "MXN"}, nil
		})
		r1, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "30", Destination: payments.RefundToOriginal, IdempotencyKey: "ret-1", CreditNoteRequired: true})
		if err != nil || r1.Refund.Status != payments.RefundSucceeded || r1.Payment.Status != payments.StatusPartiallyRefunded || r1.Payment.AmountRefunded != "30.00" {
			t.Fatalf("refund: %+v %v", r1, err)
		}
		if r1.Refund.Complete() {
			t.Fatal("invoiced refund complete without credit note")
		}
		// Retry with the same key: replay, provider not called again.
		calls := h.fake.count(capability.CommercePaymentsRefund)
		rr, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "30", Destination: payments.RefundToOriginal, IdempotencyKey: "ret-1"})
		if err != nil || !rr.Replay || h.fake.count(capability.CommercePaymentsRefund) != calls {
			t.Fatalf("replay: %+v %v", rr, err)
		}
		// Cash refund: reserved while pending, then settled by the cash desk.
		cash, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "50", Destination: payments.RefundToCash, IdempotencyKey: "ret-2"})
		if err != nil || cash.Refund.Status != payments.RefundPending {
			t.Fatalf("cash refund: %+v %v", cash, err)
		}
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "20.01", Destination: payments.RefundToWallet, IdempotencyKey: "ret-3"}); ErrorCode(err) != CodeRefundExceeds {
			t.Fatalf("over-refund while one is pending: %v", err)
		}
		set, err := h.l.SettleRefund(ctx, h.org, SettleRequest{RefundID: cash.Refund.ID, Status: payments.RefundSucceeded, CashSessionID: "sess-1"})
		if err != nil || set.Payment.AmountRefunded != "80.00" || set.Refund.CashSessionID != "sess-1" {
			t.Fatalf("settle: %+v %v", set, err)
		}
		if _, err := h.l.SettleRefund(ctx, h.org, SettleRequest{RefundID: cash.Refund.ID, Status: payments.RefundFailed}); ErrorCode(err) != CodeInvalidTransition {
			t.Fatalf("succeeded -> failed: %v", err)
		}
		note, err := h.l.SettleRefund(ctx, h.org, SettleRequest{RefundID: r1.Refund.ID, CreditNoteID: "NC-1"})
		if err != nil || !note.Refund.Complete() {
			t.Fatalf("credit note: %+v %v", note, err)
		}
		// Pending at the provider, confirmed by its webhook (amount_refunded).
		pend, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "20", Destination: payments.RefundToOriginal, IdempotencyKey: "ret-pending"})
		if err != nil || pend.Refund.Status != payments.RefundPending {
			t.Fatalf("pending refund: %+v %v", pend, err)
		}
		o := payments.Outcome{OrderRef: "SO-1", Status: payments.StatusRefunded, Amount: "20", Currency: "MXN", ProviderRef: "ORD10",
			Kind: payments.KindRefund, AmountRefunded: "100.00", EventID: "rf-evt"}
		obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourceWebhook)
		if err != nil || obs.Payment.Status != payments.StatusRefunded {
			t.Fatalf("refund webhook: %+v %v", obs, err)
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		for _, r := range doc.Refunds {
			if r.IdempotencyKey == "ret-pending" && r.Status != payments.RefundSucceeded {
				t.Fatalf("pending refund not matched: %+v", r)
			}
		}
		drawer, _ := h.l.StartCharge(ctx, h.org, func() ChargeRequest {
			c := cashCharge("d-1", "10")
			c.DocumentID = "SO-2"
			c.DocumentTotal = "10"
			return c
		}())
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: drawer.Payment.ID, Amount: "1", Destination: payments.RefundToOriginal, IdempotencyKey: "x"}); ErrorCode(err) != CodeNotRefundable {
			t.Fatalf("drawer cash to original method: %v", err)
		}
	})
}

func TestErrorCode(t *testing.T) {
	if ErrorCode(nil) != "" || ErrorCode(errors.New("x")) != "internal" || ErrorCode(failf(CodeNotFound, "y")) != CodeNotFound {
		t.Fatal("ErrorCode")
	}
}
