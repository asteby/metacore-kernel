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
// declared collected amount, that none is zero or negative, and that all are
// quantized to currency.
func CheckTenders(currency, declared string, tenders ...string) error {
	want, err := ParseMoney(declared, currency)
	if err != nil {
		return err
	}
	sum := Amount{}
	for i, t := range tenders {
		a, err := ParseMoney(t, currency)
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
	// Change is cash handed back; never more than the cash tendered.
	Change Amount
	// Rounding is a cash shortfall absorbed under the tolerance.
	Rounding Amount
	// Remaining is still owed after the collection (partial payment).
	Remaining Amount
	// Settled: nothing is owed any more.
	Settled bool
}

// Settle errors.
var (
	// ErrInvalidTolerance: a cash rounding tolerance outside
	// [0, MaxCashTolerance(currency)].
	ErrInvalidTolerance = errors.New("payments: cash rounding tolerance out of range")
	// ErrNegativeAmount: a negative total, paid or tendered amount.
	ErrNegativeAmount = errors.New("payments: negative amount")
)

// MaxCashTolerance is the hard cap of the cash rounding tolerance: less than
// one major unit of the currency (0.99 MXN, 0.999 KWD, 0 JPY). An
// organization configures its tolerance (the smallest coin its drawers or
// devices handle) inside this cap; anything above is a configuration error,
// never a way to forgive debts.
func MaxCashTolerance(currency string) Amount {
	return MustAmount("1").Sub(MinorUnit(currency))
}

// CheckCashTolerance validates an organization's tolerance for currency.
func CheckCashTolerance(tolerance, currency string) (Amount, error) {
	tol, err := ParseMoney(orZero(tolerance), currency)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %v", ErrInvalidTolerance, err)
	}
	if tol.Sign() < 0 || tol.Cmp(MaxCashTolerance(currency)) > 0 {
		return Amount{}, fmt.Errorf("%w: %s %s (max %s)", ErrInvalidTolerance, tol, currency, MaxCashTolerance(currency))
	}
	return tol, nil
}

// Collection is what a point of sale collects at once toward a document.
type Collection struct {
	Currency string
	// Total is the document total (from its stored lines) and AlreadyPaid
	// what counts as paid before (DocumentPaid), both server-side.
	Total       string
	AlreadyPaid string
	// NonCash is the card / transfer / wallet part; it can never exceed
	// what is due (there is no change on a card).
	NonCash string
	// Cash is the cash tendered; change comes only from it.
	Cash string
	// CashTolerance is the org's rounding tolerance (CheckCashTolerance).
	CashTolerance string
}

// Settle applies a collection. The non-cash part must fit in what is due
// (ErrExceedsTotal otherwise: an overcharged card is never paid back in
// cash). The cash part covers the rest: its excess is change (≤ cash
// tendered) and a shortfall up to the tolerance is absorbed as rounding
// (cash cannot be split below the smallest coin). A shortfall beyond the
// tolerance leaves Remaining > 0: the caller decides whether partial
// payments are allowed for the document.
func Settle(c Collection) (Settlement, error) {
	var s Settlement
	parse := func(v string) (Amount, error) {
		a, err := ParseMoney(orZero(v), c.Currency)
		if err == nil && a.Sign() < 0 {
			err = fmt.Errorf("%w: %s", ErrNegativeAmount, a)
		}
		return a, err
	}
	total, err := parse(c.Total)
	if err != nil {
		return s, err
	}
	paid, err := parse(c.AlreadyPaid)
	if err != nil {
		return s, err
	}
	nonCash, err := parse(c.NonCash)
	if err != nil {
		return s, err
	}
	cash, err := parse(c.Cash)
	if err != nil {
		return s, err
	}
	tol, err := CheckCashTolerance(c.CashTolerance, c.Currency)
	if err != nil {
		return s, err
	}
	if nonCash.Add(cash).Sign() <= 0 {
		return s, fmt.Errorf("%w: nothing collected", ErrTenderMismatch)
	}
	s.Due = total.Sub(paid)
	if s.Due.Sign() <= 0 {
		return s, ErrNothingDue
	}
	if nonCash.Cmp(s.Due) > 0 {
		return s, fmt.Errorf("%w: non-cash %s, due %s", ErrExceedsTotal, nonCash, s.Due)
	}
	rest := s.Due.Sub(nonCash) // what the cash must cover
	switch {
	case cash.Cmp(rest) >= 0:
		s.Applied, s.Change, s.Settled = s.Due, cash.Sub(rest), true
	case cash.Sign() > 0 && rest.Sub(cash).Cmp(tol) <= 0:
		s.Applied, s.Rounding, s.Settled = nonCash.Add(cash), rest.Sub(cash), true
	default:
		s.Applied = nonCash.Add(cash)
		s.Remaining = s.Due.Sub(s.Applied)
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

// ErrKeyRequired: a charge without an idempotency key.
var ErrKeyRequired = errors.New("payments: idempotency_key is required")

// CheckReplay decides whether a charge of a document may proceed. payments
// and refunds are every payment and refund of the document; what counts as
// paid is computed from them (DocumentPaid), never taken from the caller.
// total is the document total from its stored lines.
func CheckReplay(payments []Payment, refunds []Refund, idempotencyKey, total string) (Verdict, *Payment, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return "", nil, ErrKeyRequired
	}
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
	t, err := ParseAmount(total)
	if err != nil {
		return "", nil, err
	}
	if t.Sign() <= 0 {
		return "", nil, fmt.Errorf("%w: total %s", ErrNegativeAmount, t)
	}
	paid, err := DocumentPaid(payments, refunds)
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
