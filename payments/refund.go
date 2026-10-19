package payments

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// RefundStatus is the normalized refund status (capability.RefundStatuses).
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
	// RefundManualRequired: the provider cannot return the money (cash, BNPL);
	// the merchant settles it by hand and confirms.
	RefundManualRequired RefundStatus = "manual_required"
)

var refundTransitions = map[RefundStatus][]RefundStatus{
	RefundPending:        {RefundSucceeded, RefundFailed, RefundManualRequired},
	RefundManualRequired: {RefundSucceeded, RefundFailed},
	RefundSucceeded:      nil,
	RefundFailed:         nil,
}

// Valid reports whether s is a known refund status.
func (s RefundStatus) Valid() bool { _, ok := refundTransitions[s]; return ok }

// CanTransitionRefund reports whether from → to is a valid refund move.
func CanTransitionRefund(from, to RefundStatus) bool {
	for _, n := range refundTransitions[from] {
		if n == to {
			return true
		}
	}
	return false
}

// RefundDestination is where the money goes back.
type RefundDestination string

const (
	// RefundToOriginal: back through the payment's provider
	// (commerce.payments.refund), on the same card / account.
	RefundToOriginal RefundDestination = "original_method"
	// RefundToWallet: credited to the customer's wallet (store credit).
	RefundToWallet RefundDestination = "wallet"
	// RefundToCash: paid out in cash from an open cash session.
	RefundToCash RefundDestination = "cash"
)

// Valid reports whether d is a known destination.
func (d RefundDestination) Valid() bool {
	return d == RefundToOriginal || d == RefundToWallet || d == RefundToCash
}

// Refund is the canonical refund record, a child of one Payment. A return
// refunded on several payments (split tender) is several refunds.
type Refund struct {
	ID          string            `json:"id"`
	PaymentID   string            `json:"payment_id"`
	Destination RefundDestination `json:"destination"`
	// Provider / ProviderRefundID are set for RefundToOriginal.
	Provider         string       `json:"provider,omitempty"`
	ProviderRefundID string       `json:"provider_refund_id,omitempty"`
	Amount           string       `json:"amount"`
	Currency         string       `json:"currency"`
	Status           RefundStatus `json:"status"`
	Reason           string       `json:"reason,omitempty"`
	IdempotencyKey   string       `json:"idempotency_key"`
	// SourceType / SourceID is what caused it (sales_return, cancellation,
	// marketplace dispute ...).
	SourceType string `json:"source_type,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	// CreditNoteRequired is set when the paid document was invoiced: the
	// refund is not complete until CreditNoteID is set (fiscal rule, any
	// country with invoices: the invoice must be corrected, never the refund
	// left without a fiscal document).
	CreditNoteRequired bool   `json:"credit_note_required,omitempty"`
	CreditNoteID       string `json:"credit_note_id,omitempty"`
	// AdjustsTotal is set when the refund comes with a matching reduction
	// of the document total (a return, a credit note): only then does it
	// lower what counts as paid (DocumentPaid). A refund that does not touch
	// the total (goodwill, a refund made in the provider panel) never
	// reopens the document's debt, or a channel would charge it again.
	AdjustsTotal  bool   `json:"adjusts_total,omitempty"`
	CashSessionID string `json:"cash_session_id,omitempty"`
	// NeedsReview: still pending where the ledger can no longer ask the
	// provider safely; a human settles it (the amount stays reserved).
	NeedsReview bool       `json:"needs_review,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SettledAt   *time.Time `json:"settled_at,omitempty"`
}

// Complete reports whether nothing is left to do: money returned and, when
// required, the credit note issued.
func (r Refund) Complete() bool {
	return r.Status == RefundSucceeded && (!r.CreditNoteRequired || strings.TrimSpace(r.CreditNoteID) != "")
}

// Refund errors.
var (
	ErrNotRefundable  = errors.New("payments: payment is not refundable")
	ErrRefundExceeds  = errors.New("payments: refund exceeds the refundable amount")
	ErrRefundCurrency = errors.New("payments: refund currency differs from the payment")
	// ErrIdempotencyConflict: a refund with this idempotency key exists with
	// other data (amount, currency, destination).
	ErrIdempotencyConflict = errors.New("payments: idempotency key reused with different data")
)

func sumRefunds(p Payment, refunds []Refund, statuses ...RefundStatus) (Amount, error) {
	total := Amount{}
	for _, r := range refunds {
		if r.PaymentID != p.ID {
			continue
		}
		for _, st := range statuses {
			if r.Status == st {
				a, err := ParseAmount(r.Amount)
				if err != nil {
					return Amount{}, err
				}
				total = total.Add(a)
			}
		}
	}
	return total, nil
}

// Refundable is what can still be refunded on p: the captured amount minus
// what already went back — the larger of the provider's amount_refunded
// (which includes refunds made outside OPS, in the provider panel) and the
// succeeded refund rows — minus what may still go back (pending,
// manual_required). Failed refunds free their amount.
func Refundable(p Payment, refunds []Refund) (Amount, error) {
	if !p.Status.Counts() {
		return Amount{}, nil
	}
	amount, err := ParseAmount(p.Amount)
	if err != nil {
		return Amount{}, err
	}
	recorded := Amount{}
	if p.AmountRefunded != "" {
		if recorded, err = ParseAmount(p.AmountRefunded); err != nil {
			return Amount{}, err
		}
	}
	done, err := sumRefunds(p, refunds, RefundSucceeded)
	if err != nil {
		return Amount{}, err
	}
	inFlight, err := sumRefunds(p, refunds, RefundPending, RefundManualRequired)
	if err != nil {
		return Amount{}, err
	}
	left := amount.Sub(Max(recorded, done)).Sub(inFlight)
	if left.Sign() < 0 {
		return Amount{}, nil
	}
	return left, nil
}

// CheckRefund validates a new refund on p against its existing refunds. The
// same IdempotencyKey with the same amount, currency and destination is a
// replay (the caller returns the existing refund); with other data, or on
// another payment, it is ErrIdempotencyConflict, never a silent replay of a
// different refund.
func CheckRefund(p Payment, refunds []Refund, next Refund) (replay *Refund, err error) {
	if strings.TrimSpace(next.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency_key is required", ErrNotRefundable)
	}
	if next.Currency == "" {
		next.Currency = p.Currency
	}
	amount, err := ParseMoney(next.Amount, next.Currency)
	if err != nil {
		return nil, err
	}
	for i := range refunds {
		r := &refunds[i]
		if r.IdempotencyKey != next.IdempotencyKey {
			continue
		}
		prev, perr := ParseAmount(r.Amount)
		if perr != nil || r.PaymentID != p.ID || prev.Cmp(amount) != 0 || r.Currency != next.Currency || r.Destination != next.Destination {
			return nil, fmt.Errorf("%w: refund %s is %s %s to %s", ErrIdempotencyConflict, r.ID, r.Amount, r.Currency, r.Destination)
		}
		return r, nil
	}
	if !next.Destination.Valid() {
		return nil, fmt.Errorf("%w: destination %q", ErrNotRefundable, next.Destination)
	}
	if next.Destination == RefundToOriginal && next.Provider != "" && next.Provider != p.Provider {
		return nil, fmt.Errorf("%w: the original method is provider %s, not %s", ErrNotRefundable, p.Provider, next.Provider)
	}
	if !p.Status.Counts() {
		return nil, fmt.Errorf("%w: status %s", ErrNotRefundable, p.Status)
	}
	if next.Currency != p.Currency {
		return nil, fmt.Errorf("%w: %s vs %s", ErrRefundCurrency, next.Currency, p.Currency)
	}
	if amount.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", ErrNotRefundable)
	}
	left, err := Refundable(p, refunds)
	if err != nil {
		return nil, err
	}
	if amount.Cmp(left) > 0 {
		return nil, fmt.Errorf("%w: %s requested, %s refundable", ErrRefundExceeds, amount, left)
	}
	return nil, nil
}

// ApplySucceededRefunds raises AmountRefunded and the captured status of p to
// cover its succeeded refunds. It never LOWERS what is recorded (the
// provider's amount_refunded may include refunds made outside OPS): rows that
// add up to less only leave the payment as it is, and rows that add up to
// more than the amount are ErrRefundExceeds.
func ApplySucceededRefunds(p *Payment, refunds []Refund) error {
	if !p.Status.Captured() {
		return fmt.Errorf("%w: status %s", ErrNotRefundable, p.Status)
	}
	amount, err := ParseAmount(p.Amount)
	if err != nil {
		return err
	}
	done, err := sumRefunds(*p, refunds, RefundSucceeded)
	if err != nil {
		return err
	}
	if done.Cmp(amount) > 0 {
		return fmt.Errorf("%w: %s refunded of %s", ErrRefundExceeds, done, amount)
	}
	recorded := Amount{}
	if p.AmountRefunded != "" {
		if recorded, err = ParseAmount(p.AmountRefunded); err != nil {
			return err
		}
	}
	total := Max(recorded, done)
	if total.Sign() > 0 {
		p.AmountRefunded = total.String()
	}
	p.Status = StatusAfterRefunds(amount, total)
	return nil
}

// DocumentPaid is what counts as paid toward a document's total: every
// captured payment's amount, minus only the refunds that came with a
// reduction of the total (Refund.AdjustsTotal, succeeded or in flight). A
// refund that did not touch the total never reopens the debt.
func DocumentPaid(payments []Payment, refunds []Refund) (Amount, error) {
	paid := Amount{}
	ids := map[string]bool{}
	for _, p := range payments {
		if !p.Status.Captured() {
			continue
		}
		a, err := ParseAmount(p.Amount)
		if err != nil {
			return Amount{}, err
		}
		paid = paid.Add(a)
		ids[p.ID] = true
	}
	for _, r := range refunds {
		if !r.AdjustsTotal || !ids[r.PaymentID] || r.Status == RefundFailed {
			continue
		}
		a, err := ParseAmount(r.Amount)
		if err != nil {
			return Amount{}, err
		}
		paid = paid.Sub(a)
	}
	return paid, nil
}
