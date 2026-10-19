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
	// Same key and same data = replay of the existing refund.
	same := Refund{PaymentID: "p1", Amount: "400.00", Currency: "MXN", IdempotencyKey: "ret-1", Destination: RefundToOriginal}
	if replay, err := CheckRefund(p, existing, same); err != nil || replay == nil || replay.ID != "r1" {
		t.Fatalf("replay: %v %v", replay, err)
	}
	// r4 B5: the same key with another amount, currency, destination or
	// payment is a conflict, never a replay the caller reads as done.
	for name, r := range map[string]Refund{
		"amount":      {PaymentID: "p1", Amount: "90", Currency: "MXN", IdempotencyKey: "ret-1", Destination: RefundToOriginal},
		"destination": {PaymentID: "p1", Amount: "400", Currency: "MXN", IdempotencyKey: "ret-1", Destination: RefundToWallet},
		"payment":     {PaymentID: "p1", Amount: "1000", Currency: "MXN", IdempotencyKey: "x", Destination: RefundToOriginal},
	} {
		if replay, err := CheckRefund(p, existing, r); !errors.Is(err, ErrIdempotencyConflict) || replay != nil {
			t.Errorf("key reuse with other %s: %v %v", name, replay, err)
		}
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

// r4 B2: a refund made in the provider panel (amount_refunded observed, no
// Refund row) is not refundable again, and applying the rows never lowers it.
func TestRefundableHonorsProviderRefunded(t *testing.T) {
	p := Payment{ID: "p1", Provider: "mercadopago", Status: StatusPartiallyRefunded, Amount: "100.00", AmountRefunded: "60.00", Currency: "MXN"}
	left, err := Refundable(p, nil)
	if err != nil || left.String() != "40.00" {
		t.Fatalf("Refundable = %s, %v", left, err)
	}
	r := Refund{PaymentID: "p1", Amount: "100", Currency: "MXN", IdempotencyKey: "k", Destination: RefundToOriginal}
	if _, err := CheckRefund(p, nil, r); !errors.Is(err, ErrRefundExceeds) {
		t.Fatalf("double refund accepted: %v", err)
	}
	// Local rows above what the provider reports: the rows win.
	rows := []Refund{{PaymentID: "p1", Amount: "70", Status: RefundSucceeded}}
	if left, _ := Refundable(p, rows); left.String() != "30.00" {
		t.Fatalf("Refundable with rows = %s", left)
	}
	// Rows below the provider's total leave the payment as it is.
	small := []Refund{{PaymentID: "p1", Amount: "10", Status: RefundSucceeded}}
	if err := ApplySucceededRefunds(&p, small); err != nil || p.AmountRefunded != "60.00" || p.Status != StatusPartiallyRefunded {
		t.Fatalf("ApplySucceededRefunds lowered the provider total: %+v %v", p, err)
	}
	// In-flight refunds reserve on top of the provider total.
	pending := []Refund{{PaymentID: "p1", Amount: "40", Status: RefundPending}}
	if left, _ := Refundable(p, pending); left.Sign() != 0 {
		t.Fatalf("Refundable with pending = %s", left)
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
