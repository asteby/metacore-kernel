package payments

import "strings"

// Money-level observation reasons (ObserveOutcome).
const (
	// ReasonAmountMismatch: the provider captured (or refunded) another
	// amount than the payment's.
	ReasonAmountMismatch = "amount_mismatch"
	// ReasonCurrencyMismatch: the provider reports another currency.
	ReasonCurrencyMismatch = "currency_mismatch"
	// ReasonProviderRefMismatch: the re-read is about another provider
	// payment than the one stored.
	ReasonProviderRefMismatch = "provider_ref_mismatch"
	// ReasonInvalidOutcome: the outcome is not well-formed money.
	ReasonInvalidOutcome = "invalid_outcome"
)

// ObserveOutcome classifies a provider re-read against the stored payment:
// status (Observe) AND money. It is what a ledger applies; Observe(Status,
// Status) is its status-only core.
//
//   - another currency, another provider_ref        → Anomaly (a stored
//     payment without provider_ref yet learns it from the first re-read)
//   - an authorization or capture (kind payment) without amount → Anomaly
//     invalid_outcome
//   - an authorization or capture (paid / partially_refunded / refunded) of
//     another amount than the payment's (kind payment) → Anomaly amount_mismatch
//   - refunded total above the amount               → Anomaly amount_mismatch
//   - refunded total below the stored one           → Anomaly refund_reversed
//   - partially_refunded again with the same total  → Ignore duplicate
//   - partially_refunded again with a larger total  → Apply
//   - cancelling, observed as an open status older than the one the
//     cancellation was requested from (CancelledFrom) → Ignore stale
//   - otherwise                                     → Observe(stored.Status, o.Status)
//
// The refunded total of the outcome is o.AmountRefunded; for status refunded
// without it, the whole amount; for a kind refund update without it, the
// stored total plus o.Amount (that refund).
func ObserveOutcome(stored Payment, o Outcome) Observation {
	if !o.Status.Valid() || !stored.Status.Valid() {
		return Observation{Anomaly, ReasonUnknownStatus}
	}
	if o.Currency != "" && stored.Currency != "" && o.Currency != stored.Currency {
		return Observation{Anomaly, ReasonCurrencyMismatch}
	}
	if stored.ProviderRef != "" && o.ProviderRef != "" && strings.TrimSpace(o.ProviderRef) != stored.ProviderRef {
		return Observation{Anomaly, ReasonProviderRefMismatch}
	}
	amount, err := ParseAmount(stored.Amount)
	if err != nil {
		return Observation{Anomaly, ReasonInvalidOutcome}
	}
	kind := o.Kind
	if kind == "" {
		kind = KindPayment
	}
	if kind == KindPayment && (o.Status.Captured() || o.Status == StatusAuthorized) {
		if strings.TrimSpace(o.Amount) == "" {
			// A capture without its amount cannot be checked: never applied.
			return Observation{Anomaly, ReasonInvalidOutcome}
		}
		got, err := ParseAmount(o.Amount)
		if err != nil {
			return Observation{Anomaly, ReasonInvalidOutcome}
		}
		if got.Cmp(amount) != 0 {
			return Observation{Anomaly, ReasonAmountMismatch}
		}
	}
	storedRefunded := Amount{}
	if stored.AmountRefunded != "" {
		if storedRefunded, err = ParseAmount(stored.AmountRefunded); err != nil {
			return Observation{Anomaly, ReasonInvalidOutcome}
		}
	}
	if o.Status.Captured() {
		observed, known, err := observedRefunded(o, amount, storedRefunded)
		if err != nil {
			return Observation{Anomaly, ReasonInvalidOutcome}
		}
		if known {
			switch {
			case observed.Sign() < 0 || observed.Cmp(amount) > 0:
				return Observation{Anomaly, ReasonAmountMismatch}
			case observed.Cmp(storedRefunded) < 0:
				return Observation{Anomaly, ReasonRefundReversed}
			case StatusAfterRefunds(amount, observed) != o.Status:
				// A status that contradicts its own refunded total.
				return Observation{Anomaly, ReasonAmountMismatch}
			case stored.Status == StatusPartiallyRefunded && o.Status == StatusPartiallyRefunded:
				if observed.Cmp(storedRefunded) == 0 {
					return Observation{Ignore, ReasonDuplicate}
				}
				return Observation{Decision: Apply}
			}
		}
	}
	if staleAfterCancelling(stored, o.Status) {
		return Observation{Ignore, ReasonStale}
	}
	return Observe(stored.Status, o.Status)
}

// staleAfterCancelling reports whether observed is an open status older than
// the one a cancelling payment was in when the cancellation was requested: a
// refused cancellation goes back to that status (or a newer one), never to an
// older one (authorized → cancelling → at_device would drop the
// authorization).
func staleAfterCancelling(stored Payment, observed Status) bool {
	from := stored.CancelledFrom
	if stored.Status != StatusCancelling || !from.Open() || from == StatusCancelling {
		return false
	}
	return observed.Open() && observed != StatusCancelling && observed != from && openRank[observed] <= openRank[from]
}

// observedRefunded is the refunded total an outcome reports (known=false when
// it says nothing about it).
func observedRefunded(o Outcome, amount, storedRefunded Amount) (Amount, bool, error) {
	switch {
	case o.AmountRefunded != "":
		a, err := ParseAmount(o.AmountRefunded)
		return a, true, err
	case o.Status == StatusRefunded:
		return amount, true, nil
	case o.Status == StatusPaid:
		return Amount{}, true, nil
	case o.Kind == KindRefund && o.Amount != "":
		a, err := ParseAmount(o.Amount)
		if err != nil {
			return a, false, err
		}
		return storedRefunded.Add(a), true, nil
	}
	return Amount{}, false, nil
}
