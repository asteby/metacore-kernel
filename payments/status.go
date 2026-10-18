package payments

import (
	"errors"
	"fmt"
)

// Status is the normalized payment status (capability.PaymentStatuses).
type Status string

const (
	// StatusPending: created, nothing happened yet (session open, terminal
	// order queued, offline voucher issued).
	StatusPending Status = "pending"
	// StatusRequiresAction: the buyer must do something (3-D Secure, redirect).
	StatusRequiresAction Status = "requires_action"
	// StatusAtDevice: the amount is on an in-person device waiting for the
	// buyer (card terminal showing it, cash recycler accepting bills).
	StatusAtDevice Status = "at_device"
	// StatusAuthorized: funds reserved, not captured.
	StatusAuthorized Status = "authorized"
	// StatusPaid: captured.
	StatusPaid Status = "paid"
	// StatusFailed: rejected / errored; no money moved.
	StatusFailed Status = "failed"
	// StatusCancelling: cancellation requested and accepted by the provider but
	// not confirmed (e.g. a terminal order already on the device). The payment
	// may STILL be captured: never treat it as cancelled.
	StatusCancelling Status = "cancelling"
	// StatusCancelled: voided before capture.
	StatusCancelled Status = "cancelled"
	// StatusExpired: lapsed before capture.
	StatusExpired Status = "expired"
	// StatusRefunded: captured and fully refunded.
	StatusRefunded Status = "refunded"
	// StatusPartiallyRefunded: captured and partly refunded.
	StatusPartiallyRefunded Status = "partially_refunded"
)

// Statuses lists every status in the order of capability.PaymentStatuses.
var Statuses = []Status{
	StatusPending, StatusRequiresAction, StatusAuthorized, StatusPaid, StatusFailed,
	StatusCancelled, StatusExpired, StatusRefunded, StatusPartiallyRefunded,
	StatusAtDevice, StatusCancelling,
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, k := range Statuses {
		if k == s {
			return true
		}
	}
	return false
}

// Open reports whether money may still move into the payment: pending,
// requires_action, at_device, authorized or cancelling. A document with an
// open payment must not be charged again (see Hold) nor considered unpaid.
func (s Status) Open() bool {
	switch s {
	case StatusPending, StatusRequiresAction, StatusAtDevice, StatusAuthorized, StatusCancelling:
		return true
	}
	return false
}

// Captured reports whether the money was captured at some point: paid,
// partially_refunded or refunded.
func (s Status) Captured() bool {
	switch s {
	case StatusPaid, StatusPartiallyRefunded, StatusRefunded:
		return true
	}
	return false
}

// Final reports whether no transition leaves s: failed, cancelled, expired
// and refunded. paid and partially_refunded are not final (refunds follow).
func (s Status) Final() bool { return len(transitions[s]) == 0 }

// Counts reports whether the payment counts toward the document's paid total:
// paid and partially_refunded (net of amount_refunded). An authorized payment
// does not count until captured.
func (s Status) Counts() bool { return s == StatusPaid || s == StatusPartiallyRefunded }

// transitions is the table of valid moves. Self-moves are not listed except
// partially_refunded → partially_refunded (another partial refund).
var transitions = map[Status][]Status{
	StatusPending:        {StatusRequiresAction, StatusAtDevice, StatusAuthorized, StatusPaid, StatusFailed, StatusCancelling, StatusCancelled, StatusExpired},
	StatusRequiresAction: {StatusPending, StatusAuthorized, StatusPaid, StatusFailed, StatusCancelling, StatusCancelled, StatusExpired},
	StatusAtDevice:       {StatusAuthorized, StatusPaid, StatusFailed, StatusCancelling, StatusCancelled, StatusExpired},
	StatusAuthorized:     {StatusPaid, StatusFailed, StatusCancelling, StatusCancelled, StatusExpired},
	// A requested cancellation can lose the race (the buyer already paid on
	// the device) or be refused while the device still holds the order.
	StatusCancelling:        {StatusCancelled, StatusPaid, StatusAuthorized, StatusAtDevice, StatusFailed, StatusExpired},
	StatusPaid:              {StatusPartiallyRefunded, StatusRefunded},
	StatusPartiallyRefunded: {StatusPartiallyRefunded, StatusRefunded},
	StatusFailed:            nil,
	StatusCancelled:         nil,
	StatusExpired:           nil,
	StatusRefunded:          nil,
}

// Next lists the statuses a payment in s may move to.
func Next(s Status) []Status { return append([]Status(nil), transitions[s]...) }

// CanTransition reports whether from → to is a valid move.
func CanTransition(from, to Status) bool {
	for _, n := range transitions[from] {
		if n == to {
			return true
		}
	}
	return false
}

// ErrInvalidTransition is wrapped by Transition errors.
var ErrInvalidTransition = errors.New("payments: invalid status transition")

// Transition validates a LOCAL command (cancel requested, cash counted, staff
// confirmation): from → to must be in the table. Provider re-reads go through
// Observe instead.
func Transition(from, to Status) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("%w: unknown status %q -> %q", ErrInvalidTransition, from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

// Reachable reports whether to can follow from through one or more valid
// moves (a provider re-read may skip intermediate statuses: a missed "paid"
// webhook followed by "refunded").
func Reachable(from, to Status) bool {
	seen := map[Status]bool{from: true}
	queue := []Status{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range transitions[cur] {
			if n == to {
				return true
			}
			if !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	return false
}

// Decision is how a re-read provider status relates to the stored one.
type Decision string

const (
	// Apply: a forward move (possibly skipping intermediate statuses);
	// persist it.
	Apply Decision = "apply"
	// Ignore: the same status again, or an older one arriving late (webhooks
	// and polls cross each other); keep the stored status, still record the
	// event.
	Ignore Decision = "ignore"
	// Anomaly: the provider reports something the lifecycle does not allow.
	// Do NOT overwrite automatically: record the event, flag the payment,
	// alert, and re-read. When a later re-read (the reconciliation job) still
	// reports it, the ledger applies the provider status as an audited
	// correction: the provider decides what happened to the money.
	Anomaly Decision = "anomaly"
)

// Observation reasons.
const (
	ReasonDuplicate       = "duplicate"
	ReasonStale           = "stale"
	ReasonLateCapture     = "late_capture"
	ReasonCaptureReversed = "capture_reversed"
	ReasonRefundReversed  = "refund_reversed"
	ReasonUnknownStatus   = "unknown_status"
)

// Observation is the classification of one re-read status.
type Observation struct {
	Decision Decision
	// Reason is a stable code for Ignore and Anomaly (Reason* constants).
	Reason string
}

// Observe classifies a status re-read from the provider against the stored
// one. It never errors: the provider decides what happened to the money; the
// ledger records it and raises what needs a human.
//
//	unknown status                                    → Anomaly unknown_status
//	same status (except another partial refund)       → Ignore duplicate
//	reachable through valid moves                     → Apply
//	authorized/captured after failed/cancelled/expired → Anomaly late_capture
//	open status after a later one                     → Ignore stale
//	failed/cancelled/expired after a capture          → Anomaly capture_reversed
//	refunded (partly or fully) reported as less refunded → Anomaly refund_reversed
//	failed ↔ cancelled ↔ expired                      → Ignore stale (no money either way)
func Observe(stored, observed Status) Observation {
	gaveUp := func(s Status) bool { return s == StatusFailed || s == StatusCancelled || s == StatusExpired }
	switch {
	case !observed.Valid() || !stored.Valid():
		return Observation{Anomaly, ReasonUnknownStatus}
	case stored == observed && stored != StatusPartiallyRefunded:
		return Observation{Ignore, ReasonDuplicate}
	case Reachable(stored, observed):
		return Observation{Decision: Apply}
	case gaveUp(stored) && (observed == StatusAuthorized || observed.Captured()):
		return Observation{Anomaly, ReasonLateCapture}
	case observed.Open():
		return Observation{Ignore, ReasonStale}
	case stored.Captured() && gaveUp(observed):
		return Observation{Anomaly, ReasonCaptureReversed}
	case stored.Captured() && observed.Captured():
		// refunded → paid / partially_refunded, partially_refunded → paid.
		return Observation{Anomaly, ReasonRefundReversed}
	}
	// Both gave up (failed / cancelled / expired): the first final word stays.
	return Observation{Ignore, ReasonStale}
}
