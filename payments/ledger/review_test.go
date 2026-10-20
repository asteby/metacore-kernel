package ledger

import (
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"gorm.io/gorm"
)

// A refund that did not reduce the document total never reopens the debt;
// a return that did is consistent with the lowered total (payments.DocumentPaid).
func TestRefundDoesNotReopenDebt(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		paid, err := h.l.StartCharge(ctx, h.org, cashCharge("c-1", "100.00"))
		if err != nil || paid.Payment.Status != payments.StatusPaid {
			t.Fatalf("cash: %+v %v", paid, err)
		}
		goodwill, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: paid.Payment.ID, Amount: "30", Destination: payments.RefundToCash, IdempotencyKey: "gw-1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.l.SettleRefund(ctx, h.org, SettleRequest{RefundID: goodwill.Refund.ID, Status: payments.RefundSucceeded}); err != nil {
			t.Fatal(err)
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if doc.NetPaid != "70.00" || doc.Paid != "100.00" {
			t.Fatalf("net %s paid %s", doc.NetPaid, doc.Paid)
		}
		if r, err := h.l.StartCharge(ctx, h.org, cashCharge("c-2", "30.00")); err != nil || r.Verdict != payments.VerdictAlreadyPaid {
			t.Fatalf("goodwill refund reopened the debt: %+v %v", r, err)
		}
		// A return lowers the total: 100 → 80 with a refund of 20 that adjusts it.
		ret, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: paid.Payment.ID, Amount: "20", Destination: payments.RefundToCash, IdempotencyKey: "ret-1", AdjustsTotal: true})
		if err != nil || !ret.Refund.AdjustsTotal {
			t.Fatalf("return: %+v %v", ret, err)
		}
		doc, _ = h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if doc.Paid != "80.00" {
			t.Fatalf("paid after return %s", doc.Paid)
		}
		c := cashCharge("c-3", "10.00")
		c.DocumentTotal = "80.00"
		if r, err := h.l.StartCharge(ctx, h.org, c); err != nil || r.Verdict != payments.VerdictAlreadyPaid {
			t.Fatalf("return with lowered total: %+v %v", r, err)
		}
	})
}

// r4 B5 through the ledger: the same refund key with another amount is a
// conflict, not a replay.
func TestRefundKeyConflict(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		paid, _ := h.l.StartCharge(ctx, h.org, cashCharge("c-1", "100.00"))
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: paid.Payment.ID, Amount: "10", Destination: payments.RefundToCash, IdempotencyKey: "k"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: paid.Payment.ID, Amount: "90", Destination: payments.RefundToCash, IdempotencyKey: "k"}); ErrorCode(err) != CodeIdempotency {
			t.Fatalf("same key, other amount: %v", err)
		}
		if r, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: paid.Payment.ID, Amount: "10.00", Destination: payments.RefundToCash, IdempotencyKey: "k"}); err != nil || !r.Replay {
			t.Fatalf("same key, same data: %+v %v", r, err)
		}
	})
}

// r4 B3 through the ledger: a re-read that reports less refunded than
// recorded, or another provider_ref, is an anomaly and never applied, not
// even by a second consistent reconcile read when it is about money.
func TestObserveMoneyAnomalies(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		o := payments.Outcome{OrderRef: "SO-1", Status: payments.StatusPartiallyRefunded, Amount: "100.00", AmountRefunded: "40.00", Currency: "MXN", ProviderRef: p.ProviderRef, EventID: "e-1"}
		obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourceWebhook)
		if err != nil || obs.Decision != payments.Apply || obs.Payment.AmountRefunded != "40.00" {
			t.Fatalf("panel refund: %+v %v", obs, err)
		}
		// The provider panel refund is not refundable again (B2).
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(map[string]any) (any, error) {
			t.Error("provider called for an over-refund")
			return nil, nil
		})
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "70", Destination: payments.RefundToOriginal, IdempotencyKey: "r"}); ErrorCode(err) != CodeRefundExceeds {
			t.Fatalf("double refund: %v", err)
		}
		o.AmountRefunded, o.EventID = "10.00", "e-2"
		obs, err = h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourceWebhook)
		if err != nil || obs.Decision != payments.Anomaly || obs.Reason != payments.ReasonRefundReversed || obs.Payment.AmountRefunded != "40.00" {
			t.Fatalf("refund reversed by amount: %+v %v", obs, err)
		}
		bad := payments.Outcome{OrderRef: "SO-1", Status: payments.StatusPaid, Amount: "100.00", Currency: "USD", ProviderRef: p.ProviderRef, EventID: "e-3"}
		for i := 0; i < 2; i++ {
			bad.EventID += "x"
			obs, err = h.l.Observe(ctx, h.org, "mercadopago", "", bad, payments.SourceReconcile)
			if err != nil || obs.Decision != payments.Anomaly || obs.Reason != payments.ReasonCurrencyMismatch || obs.Corrected {
				t.Fatalf("currency mismatch read %d: %+v %v", i, obs, err)
			}
		}
	})
}
