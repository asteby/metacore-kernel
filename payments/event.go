package payments

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
)

// EventKind tells what an update is about.
type EventKind string

const (
	KindPayment EventKind = "payment"
	KindRefund  EventKind = "refund"
	KindDispute EventKind = "dispute"
)

// Source tells how an update was observed (capability.PaymentEventSources).
type Source string

const (
	SourceWebhook   Source = "webhook"
	SourcePoll      Source = "poll"
	SourceReconcile Source = "reconcile"
	SourceStaff     Source = "staff"
)

// PaymentEvent is one observed provider update of a payment, kept for dedupe
// and audit. Every webhook, poll answer and reconciliation row that touched a
// payment is one event, applied or not (Decision).
type PaymentEvent struct {
	ID        string    `json:"id"`
	PaymentID string    `json:"payment_id"`
	Provider  string    `json:"provider"`
	EventID   string    `json:"event_id"`
	Kind      EventKind `json:"kind"`
	Source    Source    `json:"source"`
	// Status is the re-read status, never the webhook body's.
	Status     Status    `json:"status"`
	Decision   Decision  `json:"decision"`
	Reason     string    `json:"reason,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// DedupeKey is unique per provider observation: the ledger keeps a unique
// index on it and an event whose key exists is not applied again.
func (e PaymentEvent) DedupeKey() string { return DedupeKey(e.Provider, e.EventID) }

// DedupeKey joins provider and event id.
func DedupeKey(provider, eventID string) string { return provider + "\x1f" + eventID }

// DerivedEventID is the event id of an update the provider did not number (a
// poll or reconciliation answer): the same observed state always derives the
// same id, so re-polling an unchanged payment is a duplicate.
func DerivedEventID(kind EventKind, providerRef string, status Status, amountRefunded string) string {
	if kind == "" {
		kind = KindPayment
	}
	return strings.Join([]string{"derived", string(kind), providerRef, string(status), amountRefunded}, "|")
}

// Confirmed is a decoded commerce.payments.confirmed delivery.
type Confirmed struct {
	Provider string `json:"provider"`
	// Emitter is the host-stamped publisher; it is the only trusted identity.
	Emitter        string  `json:"emitter"`
	Result         Outcome `json:"result"`
	OrganizationID string  `json:"organization_id,omitempty"`
	OccurredAt     string  `json:"occurred_at,omitempty"`
	Source         Source  `json:"source,omitempty"`
	PaymentID      string  `json:"payment_id,omitempty"`
	DocumentType   string  `json:"document_type,omitempty"`
	DocumentID     string  `json:"document_id,omitempty"`
}

// Outcome is the commerce.payments.confirm / commerce.payments.status output
// (and each item of commerce.payments.reconcile).
type Outcome struct {
	OrderRef       string      `json:"order_ref"`
	Status         Status      `json:"status"`
	Amount         string      `json:"amount"`
	Currency       string      `json:"currency"`
	ProviderRef    string      `json:"provider_ref"`
	EventID        string      `json:"event_id,omitempty"`
	Kind           EventKind   `json:"kind,omitempty"`
	Method         string      `json:"method,omitempty"`
	Installments   int         `json:"installments,omitempty"`
	PaidAt         string      `json:"paid_at,omitempty"`
	OccurredAt     string      `json:"occurred_at,omitempty"`
	AmountRefunded string      `json:"amount_refunded,omitempty"`
	Fee            string      `json:"fee,omitempty"`
	NetAmount      string      `json:"net_amount,omitempty"`
	TestMode       bool        `json:"test_mode,omitempty"`
	Channel        Channel     `json:"channel,omitempty"`
	SessionID      string      `json:"session_id,omitempty"`
	DeviceID       string      `json:"device_id,omitempty"`
	Card           *Card       `json:"card,omitempty"`
	Cash           *CashTender `json:"cash,omitempty"`
	Seller         *Seller     `json:"seller,omitempty"`
	ProviderStatus string      `json:"provider_status,omitempty"`
	StatusDetail   string      `json:"status_detail,omitempty"`
}

// DedupeEventID is the outcome's dedupe id: the provider's, or a derived one.
func (o Outcome) DedupeEventID() string {
	if o.EventID != "" {
		return o.EventID
	}
	return DerivedEventID(o.Kind, o.ProviderRef, o.Status, o.AmountRefunded)
}

// ErrUntrustedConfirmation is wrapped by every DecodeConfirmed rejection.
var ErrUntrustedConfirmation = errors.New("payments: untrusted commerce.payments.confirmed")

// DecodeConfirmed validates a delivered commerce.payments.confirmed payload
// for a subscriber: it must match the event contract, carry the host-stamped
// emitter, and that emitter must be the provider it claims to be (a payment
// event published by anyone else is dropped). The kernel only delivers the
// event from providers of a commerce.payments.* capability (EmitPolicy
// contract_namespace); this check closes the remaining gap of one provider
// speaking for another. It is still not proof of payment: re-read with
// commerce.payments.status pinned to the provider before acting.
func DecodeConfirmed(raw []byte) (Confirmed, error) {
	var c Confirmed
	ev, ok := capability.LookupEvent(capability.CommercePaymentsConfirmed)
	if !ok {
		return c, fmt.Errorf("%w: event contract not registered", ErrUntrustedConfirmation)
	}
	if problems := ev.ValidatePayload(raw); len(problems) > 0 {
		return c, fmt.Errorf("%w: %s", ErrUntrustedConfirmation, strings.Join(problems, "; "))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrUntrustedConfirmation, err)
	}
	switch {
	case strings.TrimSpace(c.Emitter) == "":
		return c, fmt.Errorf("%w: no host-stamped emitter", ErrUntrustedConfirmation)
	case c.Emitter != c.Provider:
		return c, fmt.Errorf("%w: emitter %q speaks for provider %q", ErrUntrustedConfirmation, c.Emitter, c.Provider)
	}
	return c, nil
}
