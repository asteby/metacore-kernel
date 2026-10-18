package payments

import (
	"errors"
	"testing"
)

func TestRefundTransitions(t *testing.T) {
	if !CanTransitionRefund(RefundPending, RefundSucceeded) || !CanTransitionRefund(RefundManualRequired, RefundSucceeded) {
		t.Fatal("forward refund moves")
	}
	if CanTransitionRefund(RefundSucceeded, RefundFailed) || CanTransitionRefund(RefundFailed, RefundPending) {
		t.Fatal("final refund moved")
	}
	if !RefundManualRequired.Valid() || RefundStatus("done").Valid() {
		t.Fatal("refund status validity")
	}
}

func TestCheckRefund(t *testing.T) {
	p := validPayment() // 1499.00 paid
	existing := []Refund{
		{ID: "r1", PaymentID: "p1", Amount: "400", Currency: "MXN", Status: RefundSucceeded, IdempotencyKey: "ret-1", Destination: RefundToOriginal},
		{ID: "r2", PaymentID: "p1", Amount: "99", Currency: "MXN", Status: RefundPending, IdempotencyKey: "ret-2", Destination: RefundToOriginal},
		{ID: "r3", PaymentID: "p1", Amount: "1000", Currency: "MXN", Status: RefundFailed, IdempotencyKey: "ret-3", Destination: RefundToOriginal},
		{ID: "x", PaymentID: "other", Amount: "1000", Currency: "MXN", Status: RefundSucceeded, IdempotencyKey: "x"},
	}
	left, err := Refundable(p, existing)
	if err != nil || left.String() != "1000.00" {
		t.Fatalf("Refundable = %s, %v (failed refunds free their amount, pending ones reserve it)", left, err)
	}
	next := Refund{PaymentID: "p1", Amount: "1000.00", Currency: "MXN", IdempotencyKey: "ret-4", Destination: RefundToWallet}
	if replay, err := CheckRefund(p, existing, next); err != nil || replay != nil {
		t.Fatalf("refund of the exact remainder: %v %v", replay, err)
	}
	next.Amount = "1000.01"
	if _, err := CheckRefund(p, existing, next); !errors.Is(err, ErrRefundExceeds) {
		t.Fatalf("over-refund: %v", err)
	}
	// Same key = replay, whatever the amount says now.
	next.IdempotencyKey = "ret-1"
	if replay, err := CheckRefund(p, existing, next); err != nil || replay == nil || replay.ID != "r1" {
		t.Fatalf("replay: %v %v", replay, err)
	}
	for name, r := range map[string]Refund{
		"currency":    {PaymentID: "p1", Amount: "1", Currency: "USD", IdempotencyKey: "a", Destination: RefundToCash},
		"destination": {PaymentID: "p1", Amount: "1", Currency: "MXN", IdempotencyKey: "b", Destination: "card"},
		"no key":      {PaymentID: "p1", Amount: "1", Currency: "MXN", Destination: RefundToCash},
		"negative":    {PaymentID: "p1", Amount: "-1", Currency: "MXN", IdempotencyKey: "c", Destination: RefundToCash},
	} {
		if _, err := CheckRefund(p, existing, r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	p.Status = StatusPending
	if _, err := CheckRefund(p, nil, Refund{PaymentID: "p1", Amount: "1", Currency: "MXN", IdempotencyKey: "d", Destination: RefundToOriginal}); !errors.Is(err, ErrNotRefundable) {
		t.Fatalf("refund of an unpaid payment: %v", err)
	}
}

func TestApplySucceededRefunds(t *testing.T) {
	p := validPayment()
	refunds := []Refund{
		{PaymentID: "p1", Amount: "499", Status: RefundSucceeded},
		{PaymentID: "p1", Amount: "1000", Status: RefundPending},
	}
	if err := ApplySucceededRefunds(&p, refunds); err != nil || p.Status != StatusPartiallyRefunded || p.AmountRefunded != "499.00" {
		t.Fatalf("partial: %+v %v", p, err)
	}
	refunds[1].Status = RefundSucceeded
	if err := ApplySucceededRefunds(&p, refunds); err != nil || p.Status != StatusRefunded || p.AmountRefunded != "1499.00" {
		t.Fatalf("full: %+v %v", p, err)
	}
	if errs := p.Validate(); len(errs) != 0 {
		t.Fatalf("refunded payment invalid: %v", errs)
	}
	refunds = append(refunds, Refund{PaymentID: "p1", Amount: "1", Status: RefundSucceeded})
	if err := ApplySucceededRefunds(&p, refunds); !errors.Is(err, ErrRefundExceeds) {
		t.Fatalf("double refund not caught: %v", err)
	}
}

// A refund of an invoiced sale is not done until its credit note exists.
func TestRefundCompleteNeedsCreditNote(t *testing.T) {
	r := Refund{Status: RefundSucceeded, CreditNoteRequired: true}
	if r.Complete() {
		t.Fatal("invoiced refund complete without credit note")
	}
	r.CreditNoteID = "NC-1"
	if !r.Complete() {
		t.Fatal("refund with credit note incomplete")
	}
	if (Refund{Status: RefundManualRequired}).Complete() {
		t.Fatal("manual refund not settled yet")
	}
}
