package payments

import (
	"errors"
	"fmt"
	"strings"
)

// Collection errors.
var (
	// ErrTenderMismatch: the tenders do not add up to the declared amount
	// (every row with an amount must have a method).
	ErrTenderMismatch = errors.New("payments: tenders do not add up to the declared amount")
	// ErrLinesMismatch: the document total is not the sum of its stored lines.
	ErrLinesMismatch = errors.New("payments: document total differs from its lines")
	// ErrExceedsTotal: a non-cash collection exceeds what is due.
	ErrExceedsTotal = errors.New("payments: collection exceeds the amount due")
	// ErrNothingDue: the document is already paid.
	ErrNothingDue = errors.New("payments: nothing is due")
)

// CheckTenders verifies that the tender amounts add up exactly to the
// declared collected amount and that none is zero or negative.
func CheckTenders(declared string, tenders ...string) error {
	want, err := ParseAmount(declared)
	if err != nil {
		return err
	}
	sum := Amount{}
	for i, t := range tenders {
		a, err := ParseAmount(t)
		if err != nil {
			return fmt.Errorf("tender %d: %w", i, err)
		}
		if a.Sign() <= 0 {
			return fmt.Errorf("%w: tender %d is %s", ErrTenderMismatch, i, a)
		}
		sum = sum.Add(a)
	}
	if sum.Cmp(want) != 0 {
		return fmt.Errorf("%w: tenders %s, declared %s", ErrTenderMismatch, sum, want)
	}
	return nil
}

// CheckLinesTotal verifies the document total is the sum of its STORED line
// totals. The amount to charge always comes from the stored document, never
// from the client.
func CheckLinesTotal(total string, lineTotals ...string) error {
	t, err := ParseAmount(total)
	if err != nil {
		return err
	}
	sum, err := Sum(lineTotals...)
	if err != nil {
		return err
	}
	if sum.Cmp(t) != 0 {
		return fmt.Errorf("%w: lines %s, total %s", ErrLinesMismatch, sum, t)
	}
	return nil
}

// Settlement is the result of applying a collection to a document.
type Settlement struct {
	// Due is what was owed before this collection.
	Due Amount
	// Applied is what this collection contributes to the document.
	Applied Amount
	// Change is cash handed back (only with cash).
	Change Amount
	// Rounding is a cash shortfall absorbed under the tolerance.
	Rounding Amount
	// Remaining is still owed after the collection (partial payment).
	Remaining Amount
	// Settled: nothing is owed any more.
	Settled bool
}

// Settle applies collected to a document whose total and already-paid amount
// are known (server-side). With cash involved, an overpayment is change and
// a shortfall up to cashTolerance is absorbed as rounding (cash cannot be
// split below the smallest coin a device or a drawer handles; the tolerance is
// an organization setting, "0" disables it). Without cash, an overpayment is
// ErrExceedsTotal. A shortfall beyond the tolerance leaves Remaining > 0: the
// caller decides whether partial payments are allowed for the document.
func Settle(total, alreadyPaid, collected string, cashInvolved bool, cashTolerance string) (Settlement, error) {
	var s Settlement
	t, err := ParseAmount(total)
	if err != nil {
		return s, err
	}
	paid, err := ParseAmount(orZero(alreadyPaid))
	if err != nil {
		return s, err
	}
	c, err := ParseAmount(collected)
	if err != nil {
		return s, err
	}
	tol, err := ParseAmount(orZero(cashTolerance))
	if err != nil {
		return s, err
	}
	if c.Sign() <= 0 {
		return s, fmt.Errorf("%w: collected must be positive", ErrTenderMismatch)
	}
	if tol.Sign() < 0 {
		tol = Amount{}
	}
	s.Due = t.Sub(paid)
	if s.Due.Sign() <= 0 {
		return s, ErrNothingDue
	}
	diff := c.Sub(s.Due)
	switch {
	case diff.Sign() >= 0:
		if diff.Sign() > 0 && !cashInvolved {
			return s, fmt.Errorf("%w: collected %s, due %s", ErrExceedsTotal, c, s.Due)
		}
		s.Applied, s.Change, s.Settled = s.Due, diff, true
	case cashInvolved && c.Sub(s.Due).Add(tol).Sign() >= 0:
		s.Applied, s.Rounding, s.Settled = c, s.Due.Sub(c), true
	default:
		s.Applied, s.Remaining = c, s.Due.Sub(c)
	}
	return s, nil
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// Verdict is what to do with a charge request given the document's payments.
type Verdict string

const (
	// VerdictNew: charge.
	VerdictNew Verdict = "new"
	// VerdictReplay: this very request (same idempotency key) already got
	// paid — a retry after a timeout. Answer success with the original
	// payment; never charge again.
	VerdictReplay Verdict = "replay"
	// VerdictAlreadyPaid: the document is fully paid by other payments.
	// Answer success-shaped ("already paid": stop charging, nothing was
	// charged now) with the payments; never charge again.
	VerdictAlreadyPaid Verdict = "already_paid"
	// VerdictInProgress: a payment of the document (this request's or
	// another) is still open (on a device, awaiting the buyer, cancelling).
	// Resume or poll it; never create a second one.
	VerdictInProgress Verdict = "in_progress"
	// VerdictRetryNewKey: this request's key belongs to an attempt that
	// failed / was cancelled / expired; a new attempt needs a new key (the
	// provider would answer the old outcome for the old key).
	VerdictRetryNewKey Verdict = "retry_with_new_key"
)

// CheckReplay decides whether a charge of a document may proceed. payments are
// every payment of the document; netPaid is what they already settled
// (sum of NetCaptured) and total the document total, both server-side.
func CheckReplay(payments []Payment, idempotencyKey string, netPaid, total string) (Verdict, *Payment, error) {
	if strings.TrimSpace(idempotencyKey) != "" {
		for i := range payments {
			p := &payments[i]
			if p.IdempotencyKey != idempotencyKey {
				continue
			}
			switch {
			case p.Status.Captured():
				return VerdictReplay, p, nil
			case p.Status.Open():
				return VerdictInProgress, p, nil
			default:
				return VerdictRetryNewKey, p, nil
			}
		}
	}
	paid, err := ParseAmount(orZero(netPaid))
	if err != nil {
		return "", nil, err
	}
	t, err := ParseAmount(total)
	if err != nil {
		return "", nil, err
	}
	if paid.Cmp(t) >= 0 {
		return VerdictAlreadyPaid, nil, nil
	}
	for i := range payments {
		if payments[i].Status.Open() {
			return VerdictInProgress, &payments[i], nil
		}
	}
	return VerdictNew, nil, nil
}
