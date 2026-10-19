package ledger

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// r4 #512 I1: the provider's webhook (or a re-query) settles the refund
// before its synchronous answer, which still says pending: the late answer
// never takes the refund back.
func TestLatePendingKeepsSucceededRefund(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(in map[string]any) (any, error) {
			if err := db.Model(&refundRow{}).Where("idempotency_key = ?", in["idempotency_key"]).
				Update("status", string(payments.RefundSucceeded)).Error; err != nil {
				t.Error(err)
			}
			return map[string]any{"refund_id": "rf-1", "status": "pending"}, nil
		})
		res, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "30", Destination: payments.RefundToOriginal, IdempotencyKey: "r-1"})
		if err != nil || res.Refund.Status != payments.RefundSucceeded || res.Refund.ProviderRefundID != "rf-1" {
			t.Fatalf("late pending: %+v %v", res.Refund, err)
		}
		if _, err := h.l.updateRefund(ctx, h.org, mustUUID(t, res.Refund.ID), payments.RefundFailed, "", "", "late failure"); err != nil {
			t.Fatal(err)
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if doc.Refunds[0].Status != payments.RefundSucceeded {
			t.Fatalf("succeeded refund moved: %+v", doc.Refunds[0])
		}
	})
}

// r4 #512 I2: a refund left pending (timeout, no webhook) is asked again by a
// retry with the same key and by the reconciliation job.
func TestPendingRefundIsRechecked(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		answers := map[string][]any{
			"retry": {timeoutErr(), "succeeded"},
			"recon": {timeoutErr(), "pending"},
		}
		h.fake.on("mercadopago", capability.CommercePaymentsRefundStatus, func(in map[string]any) (any, error) {
			return map[string]any{"refund_id": in["refund_id"], "status": "succeeded", "amount": "20", "currency": "MXN"}, nil
		})
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(in map[string]any) (any, error) {
			key := in["idempotency_key"].(string)
			next := answers[key][0]
			if len(answers[key]) > 1 {
				answers[key] = answers[key][1:]
			}
			if err, ok := next.(error); ok {
				return nil, err
			}
			return map[string]any{"refund_id": "rf-" + key, "status": next}, nil
		})
		first, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "10", Destination: payments.RefundToOriginal, IdempotencyKey: "retry"})
		if err != nil || !first.Indeterminate || first.Refund.Status != payments.RefundPending {
			t.Fatalf("indeterminate: %+v %v", first, err)
		}
		again, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "10", Destination: payments.RefundToOriginal, IdempotencyKey: "retry"})
		if err != nil || !again.Replay || again.Refund.Status != payments.RefundSucceeded || again.Payment.AmountRefunded != "10.00" {
			t.Fatalf("retry did not ask again: %+v %v", again, err)
		}

		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "20", Destination: payments.RefundToOriginal, IdempotencyKey: "recon"}); err != nil {
			t.Fatal(err)
		}
		rec := &Reconciler{Ledger: h.l, Owner: "replica-a"}
		// Still in flight (no provider id, just created): left alone.
		if rep, err := rec.RunOrg(ctx, h.org); err != nil || rep.Refunds != 0 {
			t.Fatalf("in-flight refund rechecked: %+v %v", rep, err)
		}
		// Not numbered, inside the window: the same request again (re-POST).
		h.advance(2 * time.Minute)
		posts := h.fake.count(capability.CommercePaymentsRefund)
		if rep, err := rec.RunOrg(ctx, h.org); err != nil || rep.Refunds != 1 || h.fake.count(capability.CommercePaymentsRefund) != posts+1 {
			t.Fatalf("run 1: %+v %v", rep, err)
		}
		// Numbered now (rf-recon): only read, never POSTed again.
		h.advance(time.Minute)
		if rep, err := rec.RunOrg(ctx, h.org); err != nil || rep.Refunds != 1 ||
			h.fake.count(capability.CommercePaymentsRefund) != posts+1 || h.fake.count(capability.CommercePaymentsRefundStatus) != 1 {
			t.Fatalf("run 2: %+v %v", rep, err)
		}
		pay, _ := h.l.Payment(ctx, h.org, p.ID)
		if pay.AmountRefunded != "30.00" || pay.Status != payments.StatusPartiallyRefunded {
			t.Fatalf("after reconcile: %+v", pay)
		}
	})
}

// r4 #512 I3: two partial refunds observed without event id nor refunded
// total (30, then 20) are two updates; a re-poll of the second is a duplicate.
func TestPartialRefundsWithoutEventID(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		for i, amount := range []string{"30", "20", "20.00"} {
			o := payments.Outcome{OrderRef: "SO-1", Kind: payments.KindRefund, Status: payments.StatusPartiallyRefunded,
				Amount: amount, Currency: "MXN", ProviderRef: "ORD10"}
			obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourcePoll)
			if err != nil || obs.Duplicate != (i == 2) {
				t.Fatalf("refund %s: %+v %v", amount, obs, err)
			}
		}
		pay, _ := h.l.Payment(ctx, h.org, p.ID)
		if pay.AmountRefunded != "50.00" {
			t.Fatalf("amount_refunded %s, want 50.00", pay.AmountRefunded)
		}
	})
}

// r4 #512 M2: a refund made in the provider's panel does not settle one of
// ours the provider has not executed: the provider is asked about ours.
func TestPanelRefundDoesNotSettleOurs(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(in map[string]any) (any, error) {
			return map[string]any{"refund_id": "rf-ours", "status": "pending"}, nil
		})
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "30", Destination: payments.RefundToOriginal, IdempotencyKey: "ours"}); err != nil {
			t.Fatal(err)
		}
		panel := payments.Outcome{OrderRef: "SO-1", Status: payments.StatusPartiallyRefunded, Amount: "100", AmountRefunded: "30",
			Currency: "MXN", ProviderRef: "ORD10", EventID: "panel-1"}
		obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", panel, payments.SourceWebhook)
		if err != nil || obs.Payment.AmountRefunded != "30.00" {
			t.Fatalf("panel refund: %+v %v", obs, err)
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if doc.Refunds[0].Status != payments.RefundPending {
			t.Fatalf("our refund settled by the panel's: %+v", doc.Refunds[0])
		}
		// Reserved: the provider's 30 plus our pending 30.
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "40.01", Destination: payments.RefundToCash, IdempotencyKey: "more"}); ErrorCode(err) != CodeRefundExceeds {
			t.Fatalf("over-refund: %v", err)
		}
	})
}

// r4 #512 M1: concurrent deliveries of the same orphan event are one record
// and duplicates, never a 500 from the unique index.
func TestConcurrentOrphanDuplicates(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		o := payments.Outcome{OrderRef: "X-9", Status: payments.StatusPaid, Amount: "50", Currency: "MXN", ProviderRef: "unknown", EventID: "evt-orphan"}
		var wg sync.WaitGroup
		var mu sync.Mutex
		orphans, dups := 0, 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				obs, err := h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourceWebhook)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					t.Errorf("observe: %v", err)
				case obs.Duplicate:
					dups++
				case obs.Orphan:
					orphans++
				}
			}()
		}
		wg.Wait()
		if orphans != 1 || dups != 7 {
			t.Fatalf("orphans=%d duplicates=%d", orphans, dups)
		}
	})
}

// r4 #507 M1 through the ledger: a cancellation the provider refuses does
// not drop the authorization a late at_device poll would undo.
func TestRefusedCancelKeepsAuthorization(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(map[string]any) (any, error) {
			return map[string]any{"provider_ref": "ORD03", "status": "authorized", "amount": "100.00"}, nil
		})
		h.fake.on("mercadopago", capability.CommercePaymentsCancel, func(map[string]any) (any, error) {
			return map[string]any{"status": "cancelling"}, nil
		})
		status := "at_device"
		h.fake.on("mercadopago", capability.CommercePaymentsStatus, func(map[string]any) (any, error) {
			return statusOf("ORD03", status, "100.00"), nil
		})
		res, err := h.l.StartCharge(ctx, h.org, terminalCharge("a-1"))
		if err != nil || res.Payment.Status != payments.StatusAuthorized {
			t.Fatalf("authorized: %+v %v", res.Payment, err)
		}
		obs, err := h.l.Cancel(ctx, h.org, res.Payment.ID, "cancel-1", "")
		if err != nil || obs.Payment.Status != payments.StatusCancelling || obs.Reason != payments.ReasonStale ||
			obs.Payment.CancelledFrom != payments.StatusAuthorized {
			t.Fatalf("late at_device after cancelling: %+v %v", obs, err)
		}
		status = "authorized"
		h.advance(time.Minute)
		if obs, err := h.l.Refresh(ctx, h.org, res.Payment.ID, payments.SourcePoll); err != nil || obs.Payment.Status != payments.StatusAuthorized {
			t.Fatalf("refused cancel: %+v %v", obs, err)
		}
	})
}

// r4 #512 M3: a long run renews its lease, and stops once another replica
// took the org over.
func TestReconcileLeaseRenewed(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(in map[string]any) (any, error) {
			return map[string]any{"provider_ref": "ORD-" + in["idempotency_key"].(string), "status": "at_device"}, nil
		})
		var rec *Reconciler
		steal := false
		h.fake.on("mercadopago", capability.CommercePaymentsStatus, func(in map[string]any) (any, error) {
			h.advance(2 * time.Minute) // a slow provider
			if steal {
				db.Model(&leaseRow{}).Where("organization_id = ?", h.org).Updates(map[string]any{"locked_by": "replica-b"})
			}
			return statusOf(in["provider_ref"].(string), "at_device", "100.00"), nil
		})
		for i := 0; i < 4; i++ {
			req := terminalCharge("l" + strconv.Itoa(i))
			req.DocumentID = "SO-L" + strconv.Itoa(i)
			if _, err := h.l.StartCharge(ctx, h.org, req); err != nil {
				t.Fatal(err)
			}
		}
		h.advance(time.Minute)
		rec = &Reconciler{Ledger: h.l, Owner: "replica-a", Lease: 5 * time.Minute, StuckAfter: time.Hour}
		var renewedUntil *time.Time
		h.fake.on("mercadopago", capability.CommercePaymentsReconcile, func(map[string]any) (any, error) {
			var l leaseRow
			db.Where("organization_id = ?", h.org).Take(&l)
			renewedUntil = l.LockedUntil
			return map[string]any{"payments": []any{}}, nil
		})
		rep, err := rec.RunOrg(ctx, h.org)
		if err != nil || rep.Refreshed != 4 || len(rep.Errors) != 0 {
			t.Fatalf("long run: %+v %v", rep, err)
		}
		if renewedUntil == nil || !renewedUntil.After(h.now) {
			t.Fatalf("lease not renewed: until %v, now %v", renewedUntil, h.now)
		}
		steal = true
		h.advance(time.Minute)
		rep, err = rec.RunOrg(ctx, h.org)
		if err != nil || rep.Refreshed >= 4 || len(rep.Errors) == 0 {
			t.Fatalf("run kept going without its lease: %+v %v", rep, err)
		}
	})
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// r4 #512 I4: an unnumbered refund is re-POSTed only inside the provider's
// window (keys expire); past it, it goes to manual review and is never asked
// again. Between asks, exponential backoff.
func TestRefundRepostWindowAndBackoff(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.l.cfg.RefundRepostWindow = map[string]time.Duration{"mercadopago": 3 * time.Hour}
		p := paidTerminal(t, h)
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(map[string]any) (any, error) {
			return nil, timeoutErr()
		})
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "10", Destination: payments.RefundToOriginal, IdempotencyKey: "w-1"}); err != nil {
			t.Fatal(err)
		}
		rec := &Reconciler{Ledger: h.l, Owner: "r"}
		posts := func() int { return h.fake.count(capability.CommercePaymentsRefund) }
		// Backoff: due after 1 min (in flight), then 1, 2, 4 ... min.
		var asked []time.Duration
		start := h.now
		for h.now.Sub(start) < 40*time.Minute {
			h.advance(30 * time.Second)
			before := posts()
			if _, err := rec.RunOrg(ctx, h.org); err != nil {
				t.Fatal(err)
			}
			if posts() > before {
				asked = append(asked, h.now.Sub(start))
			}
		}
		want := []time.Duration{90 * time.Second, 150 * time.Second, 270 * time.Second, 510 * time.Second, 990 * time.Second, 1950 * time.Second}
		if len(asked) != len(want) {
			t.Fatalf("asked at %v, want %v", asked, want)
		}
		for i := range want {
			if asked[i] != want[i] {
				t.Fatalf("asked at %v, want %v", asked, want)
			}
		}
		// Past the provider's window: needs_review, anomaly, no more POSTs.
		h.advance(3 * time.Hour)
		before := posts()
		if _, err := rec.RunOrg(ctx, h.org); err != nil {
			t.Fatal(err)
		}
		h.advance(2 * time.Hour)
		if _, err := rec.RunOrg(ctx, h.org); err != nil {
			t.Fatal(err)
		}
		if posts() != before {
			t.Fatalf("re-POSTed past the window: %d -> %d", before, posts())
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if !doc.Refunds[0].NeedsReview || doc.Refunds[0].Status != payments.RefundPending {
			t.Fatalf("not in review: %+v", doc.Refunds[0])
		}
		raised := 0
		for _, a := range h.anomalies {
			if a.Reason == ReasonRefundNeedsReview {
				raised++
			}
		}
		if raised != 1 {
			t.Fatalf("needs_review raised %d times", raised)
		}
		// A retry with the same key does not re-POST either.
		if r, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "10", Destination: payments.RefundToOriginal, IdempotencyKey: "w-1"}); err != nil || !r.Replay || posts() != before {
			t.Fatalf("retry past the window: %+v %v", r, err)
		}
		// A human settles it and the flag clears.
		set, err := h.l.SettleRefund(ctx, h.org, SettleRequest{RefundID: doc.Refunds[0].ID, Status: payments.RefundFailed})
		if err != nil || set.Refund.NeedsReview || set.Refund.Status != payments.RefundFailed {
			t.Fatalf("manual settle: %+v %v", set.Refund, err)
		}
	})
}

// r4 #512 I4 (a): once the provider numbered a refund, a retry with the same
// key reads it (refund.status) and never sends the refund again; a provider
// without refund.status leaves it pending.
func TestNumberedRefundIsOnlyRead(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(map[string]any) (any, error) {
			return map[string]any{"refund_id": "rf-9", "status": "pending", "amount": "10", "currency": "MXN"}, nil
		})
		req := RefundRequest{PaymentID: p.ID, Amount: "10", Destination: payments.RefundToOriginal, IdempotencyKey: "n-1"}
		if _, err := h.l.CreateRefund(ctx, h.org, req); err != nil {
			t.Fatal(err)
		}
		// No refund.status: still pending, no second POST.
		r, err := h.l.CreateRefund(ctx, h.org, req)
		if err != nil || !r.Replay || r.Refund.Status != payments.RefundPending || h.fake.count(capability.CommercePaymentsRefund) != 1 {
			t.Fatalf("retry without refund.status: %+v %v", r, err)
		}
		h.fake.on("mercadopago", capability.CommercePaymentsRefundStatus, func(in map[string]any) (any, error) {
			if in["refund_id"] != "rf-9" {
				t.Errorf("refund.status input %v", in)
			}
			return map[string]any{"refund_id": "rf-9", "status": "succeeded", "amount": "10", "currency": "MXN"}, nil
		})
		r, err = h.l.CreateRefund(ctx, h.org, req)
		if err != nil || r.Refund.Status != payments.RefundSucceeded || r.Payment.AmountRefunded != "10.00" || h.fake.count(capability.CommercePaymentsRefund) != 1 {
			t.Fatalf("retry read: %+v %v", r, err)
		}
	})
}

// r4 #512: the re-POST window never freezes a numbered refund. A refund the
// provider numbered stays pending 13 h (SPEI) and is still read with
// backoff; when the provider answers succeeded, it settles and the
// reservation is released.
func TestNumberedRefundReadPastWindow(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(map[string]any) (any, error) {
			return map[string]any{"refund_id": "rf-spei", "status": "pending", "amount": "60", "currency": "MXN"}, nil
		})
		status := "pending"
		h.fake.on("mercadopago", capability.CommercePaymentsRefundStatus, func(in map[string]any) (any, error) {
			return map[string]any{"refund_id": in["refund_id"], "status": status, "amount": "60", "currency": "MXN"}, nil
		})
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "60", Destination: payments.RefundToOriginal, IdempotencyKey: "spei"}); err != nil {
			t.Fatal(err)
		}
		rec := &Reconciler{Ledger: h.l, Owner: "r"}
		for i := 0; i < 13; i++ {
			h.advance(time.Hour)
			if _, err := rec.RunOrg(ctx, h.org); err != nil {
				t.Fatal(err)
			}
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if r := doc.Refunds[0]; r.Status != payments.RefundPending || r.NeedsReview {
			t.Fatalf("numbered refund frozen after 13 h: %+v", r)
		}
		reads := h.fake.count(capability.CommercePaymentsRefundStatus)
		if reads < 13 || h.fake.count(capability.CommercePaymentsRefund) != 1 {
			t.Fatalf("reads=%d posts=%d", reads, h.fake.count(capability.CommercePaymentsRefund))
		}
		// Reserved while pending: only 40 left.
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "40.01", Destination: payments.RefundToCash, IdempotencyKey: "x"}); ErrorCode(err) != CodeRefundExceeds {
			t.Fatalf("reservation: %v", err)
		}
		status = "succeeded"
		h.advance(time.Hour)
		if _, err := rec.RunOrg(ctx, h.org); err != nil {
			t.Fatal(err)
		}
		doc, _ = h.l.Document(ctx, h.org, "sales_order", "SO-1")
		pay, _ := h.l.Payment(ctx, h.org, p.ID)
		if doc.Refunds[0].Status != payments.RefundSucceeded || pay.AmountRefunded != "60.00" {
			t.Fatalf("not settled: %+v %+v", doc.Refunds[0], pay)
		}
		// The reservation became the refund: exactly 40 more can be refunded.
		if _, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "40", Destination: payments.RefundToCash, IdempotencyKey: "y"}); err != nil {
			t.Fatalf("remaining 40: %v", err)
		}
	})
}

func TestRefundBackoffCap(t *testing.T) {
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour, time.Hour} {
		if got := refundBackoff(i); got != want {
			t.Fatalf("backoff(%d) = %v, want %v", i, got, want)
		}
	}
}

// r4 #512: a late provider answer numbers the refund between the selection
// and the needs_review update: the update does not apply and no review is
// raised; the refund is read from then on.
func TestNeedsReviewLosesToLateRef(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		p := paidTerminal(t, h)
		h.fake.on("mercadopago", capability.CommercePaymentsRefund, func(map[string]any) (any, error) {
			return nil, timeoutErr()
		})
		res, err := h.l.CreateRefund(ctx, h.org, RefundRequest{PaymentID: p.ID, Amount: "10", Destination: payments.RefundToOriginal, IdempotencyKey: "late"})
		if err != nil {
			t.Fatal(err)
		}
		var stale refundRow
		if err := db.Where("id = ?", res.Refund.ID).Take(&stale).Error; err != nil {
			t.Fatal(err)
		}
		// The late synchronous answer lands after the row was read.
		if _, err := h.l.updateRefund(ctx, h.org, stale.ID, payments.RefundPending, "rf-late", "", ""); err != nil {
			t.Fatal(err)
		}
		h.advance(13 * time.Hour)
		pay, err := h.l.paymentRow(db, h.org, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		out, err := h.l.recheckRefund(ctx, h.org, pay, stale)
		if err != nil || out.Refund.NeedsReview || out.Refund.ProviderRefundID != "rf-late" {
			t.Fatalf("review over a numbered refund: %+v %v", out.Refund, err)
		}
		for _, a := range h.anomalies {
			if a.Reason == ReasonRefundNeedsReview {
				t.Fatalf("needs_review raised: %+v", a)
			}
		}
	})
}
