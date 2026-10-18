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
	CreditNoteRequired bool       `json:"credit_note_required,omitempty"`
	CreditNoteID       string     `json:"credit_note_id,omitempty"`
	CashSessionID      string     `json:"cash_session_id,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	SettledAt          *time.Time `json:"settled_at,omitempty"`
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
)

// Refundable is what can still be refunded on p given its refunds: the
// captured amount minus every refund that returned or may still return money
// (succeeded, pending, manual_required). Failed refunds free their amount.
func Refundable(p Payment, refunds []Refund) (Amount, error) {
	if !p.Status.Counts() {
		return Amount{}, nil
	}
	amount, err := ParseAmount(p.Amount)
	if err != nil {
		return Amount{}, err
	}
	reserved := Amount{}
	for _, r := range refunds {
		if r.PaymentID != p.ID || r.Status == RefundFailed {
			continue
		}
		a, err := ParseAmount(r.Amount)
		if err != nil {
			return Amount{}, err
		}
		reserved = reserved.Add(a)
	}
	left := amount.Sub(reserved)
	if left.Sign() < 0 {
		return Amount{}, nil
	}
	return left, nil
}

// CheckRefund validates a new refund of amount on p against its existing
// refunds. A refund with an IdempotencyKey already present is a replay: the
// caller returns the existing refund instead of creating one (replay=true).
func CheckRefund(p Payment, refunds []Refund, next Refund) (replay *Refund, err error) {
	for i := range refunds {
		if refunds[i].PaymentID == p.ID && next.IdempotencyKey != "" && refunds[i].IdempotencyKey == next.IdempotencyKey {
			return &refunds[i], nil
		}
	}
	if !next.Destination.Valid() {
		return nil, fmt.Errorf("%w: destination %q", ErrNotRefundable, next.Destination)
	}
	if strings.TrimSpace(next.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency_key is required", ErrNotRefundable)
	}
	if !p.Status.Counts() {
		return nil, fmt.Errorf("%w: status %s", ErrNotRefundable, p.Status)
	}
	if next.Currency != p.Currency {
		return nil, fmt.Errorf("%w: %s vs %s", ErrRefundCurrency, next.Currency, p.Currency)
	}
	amount, err := ParseAmount(next.Amount)
	if err != nil {
		return nil, err
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

// ApplySucceededRefunds recomputes AmountRefunded and the captured status of
// p from its succeeded refunds (the only ones that returned money).
func ApplySucceededRefunds(p *Payment, refunds []Refund) error {
	if !p.Status.Captured() {
		return fmt.Errorf("%w: status %s", ErrNotRefundable, p.Status)
	}
	amount, err := ParseAmount(p.Amount)
	if err != nil {
		return err
	}
	done := Amount{}
	for _, r := range refunds {
		if r.PaymentID != p.ID || r.Status != RefundSucceeded {
			continue
		}
		a, err := ParseAmount(r.Amount)
		if err != nil {
			return err
		}
		done = done.Add(a)
	}
	if done.Cmp(amount) > 0 {
		return fmt.Errorf("%w: %s refunded of %s", ErrRefundExceeds, done, amount)
	}
	p.AmountRefunded = done.String()
	p.Status = StatusAfterRefunds(amount, done)
	return nil
}
