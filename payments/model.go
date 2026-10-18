package payments

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
)

// Channel is how the buyer pays (capability.PaymentChannels).
type Channel string

const (
	// ChannelTerminal: an in-person device (card terminal, cash recycler).
	ChannelTerminal Channel = "terminal"
	// ChannelOnline: checkout, embedded widget or offline voucher.
	ChannelOnline Channel = "online"
	// ChannelQR: the buyer scans a QR (on the customer display, kiosk, ticket).
	ChannelQR Channel = "qr"
	// ChannelLink: a payment link sent to the buyer.
	ChannelLink Channel = "link"
	// ChannelCash: cash counted by staff (drawer), no device.
	ChannelCash Channel = "cash"
)

// Valid reports whether c is a known channel.
func (c Channel) Valid() bool {
	for _, k := range capability.PaymentChannels {
		if string(c) == k {
			return true
		}
	}
	return false
}

// InPerson reports whether the buyer is physically at the point of sale.
func (c Channel) InPerson() bool { return c == ChannelTerminal || c == ChannelCash }

// ProviderCash is the provider key of drawer cash recorded by staff: there is
// no provider API to re-read, the channel's count is the truth.
const ProviderCash = "cash"

// Card is what a card payment reports (capability.CardSchema).
type Card struct {
	Type      string `json:"type,omitempty"`
	Brand     string `json:"brand,omitempty"`
	Last4     string `json:"last4,omitempty"`
	AuthCode  string `json:"auth_code,omitempty"`
	EntryMode string `json:"entry_mode,omitempty"`
}

// Seller routes a marketplace payment (capability.SellerSchema).
type Seller struct {
	AccountRef     string `json:"account_ref"`
	SellerID       string `json:"seller_id,omitempty"`
	ApplicationFee string `json:"application_fee,omitempty"`
}

// CashTender is what a cash device or a counted drawer reports.
type CashTender struct {
	Tendered    string `json:"tendered,omitempty"`
	Change      string `json:"change,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	// Rounding is the shortfall absorbed under the org's cash rounding
	// tolerance (see Settle); "0.00" when none.
	Rounding string `json:"rounding,omitempty"`
}

// Payment is the canonical payment record: one attempt to collect an amount
// for one document through one provider. A split tender (cash + card) is
// several payments of the same document.
type Payment struct {
	ID string `json:"id"`
	// DocumentType / DocumentID is what is being paid (sales_order, checkout,
	// invoice, layaway...). OrderRef is the reference sent to the provider.
	DocumentType string `json:"document_type"`
	DocumentID   string `json:"document_id"`
	OrderRef     string `json:"order_ref,omitempty"`

	Channel  Channel `json:"channel"`
	Provider string  `json:"provider"`
	// ProviderRef is the provider payment / terminal order id; SessionID the
	// session / preference it came from; DeviceID the device that took it.
	ProviderRef string `json:"provider_ref,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	// IdempotencyKey is stable per business operation (one charge attempt of
	// the document) and reused on every retry.
	IdempotencyKey string `json:"idempotency_key"`

	Status         Status `json:"status"`
	Amount         string `json:"amount"`
	Currency       string `json:"currency"`
	AmountRefunded string `json:"amount_refunded,omitempty"`
	Fee            string `json:"fee,omitempty"`
	Net            string `json:"net,omitempty"`

	Method       string      `json:"method,omitempty"`
	Installments int         `json:"installments,omitempty"`
	Card         *Card       `json:"card,omitempty"`
	Cash         *CashTender `json:"cash,omitempty"`
	Seller       *Seller     `json:"seller,omitempty"`
	TestMode     bool        `json:"test_mode,omitempty"`

	// HoldToken is the document hold the charge was taken under (Hold).
	HoldToken string `json:"hold_token,omitempty"`
	// Anomaly is the last unresolved Observe reason (late_capture, ...).
	Anomaly string `json:"anomaly,omitempty"`

	PaidAt    *time.Time `json:"paid_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

var last4Re = regexp.MustCompile(`^[0-9]{4}$`)

// Validate checks the record's invariants and returns every problem.
func (p Payment) Validate() []string {
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }

	if blank(p.DocumentType) || blank(p.DocumentID) {
		bad("document_type and document_id are required")
	}
	if !p.Channel.Valid() {
		bad("channel %q is not one of %v", p.Channel, capability.PaymentChannels)
	}
	if blank(p.Provider) {
		bad("provider is required")
	}
	if blank(p.IdempotencyKey) {
		bad("idempotency_key is required")
	}
	if !p.Status.Valid() {
		bad("status %q is unknown", p.Status)
	}
	if !capability.ValidCurrency(p.Currency) {
		bad("currency %q is not ISO 4217", p.Currency)
	}
	if blank(p.ProviderRef) && (p.Status.Captured() || p.Status == StatusAuthorized) {
		bad("provider_ref is required once the payment is %s", p.Status)
	}
	if (p.Channel == ChannelCash) != (p.Provider == ProviderCash) {
		bad("channel cash (drawer counted by staff) goes with provider %q and only with it; a cash recycler is channel terminal", ProviderCash)
	}

	amount, err := ParseAmount(p.Amount)
	if err != nil {
		bad("amount: %v", err)
		return errs
	}
	if amount.Sign() <= 0 {
		bad("amount must be positive")
	}
	refunded := Amount{}
	if p.AmountRefunded != "" {
		if refunded, err = ParseAmount(p.AmountRefunded); err != nil {
			bad("amount_refunded: %v", err)
			return errs
		}
	}
	switch {
	case refunded.Sign() < 0:
		bad("amount_refunded cannot be negative")
	case refunded.Cmp(amount) > 0:
		bad("amount_refunded %s exceeds amount %s", refunded, amount)
	case p.Status.Valid():
		if want := StatusAfterRefunds(amount, refunded); p.Status.Captured() && want != p.Status {
			bad("status %s does not match amount_refunded %s of %s (want %s)", p.Status, refunded, amount, want)
		}
		if !p.Status.Captured() && refunded.Sign() != 0 {
			bad("a %s payment cannot have refunds", p.Status)
		}
	}
	if p.Fee != "" {
		fee, err := ParseAmount(p.Fee)
		switch {
		case err != nil:
			bad("fee: %v", err)
		case fee.Sign() < 0 || fee.Cmp(amount) > 0:
			bad("fee %s must be within [0, amount]", fee)
		case p.Net != "":
			net, err := ParseAmount(p.Net)
			if err != nil {
				bad("net: %v", err)
			} else if net.Cmp(amount.Sub(fee)) != 0 {
				bad("net %s must equal amount - fee (%s)", net, amount.Sub(fee))
			}
		}
	}
	if p.Card != nil {
		if p.Card.Type != "" && !oneOf(p.Card.Type, capability.CardTypes) {
			bad("card.type %q is not one of %v", p.Card.Type, capability.CardTypes)
		}
		if p.Card.Last4 != "" && !last4Re.MatchString(p.Card.Last4) {
			bad("card.last4 must be four digits")
		}
	}
	if p.Seller != nil {
		if blank(p.Seller.AccountRef) {
			bad("seller.account_ref is required")
		}
		if p.Seller.ApplicationFee != "" {
			fee, err := ParseAmount(p.Seller.ApplicationFee)
			if err != nil {
				bad("seller.application_fee: %v", err)
			} else if fee.Sign() < 0 || fee.Cmp(amount) > 0 {
				bad("seller.application_fee %s must be within [0, amount]", fee)
			}
		}
	}
	return errs
}

// StatusAfterRefunds is the captured status that matches the refunded total.
func StatusAfterRefunds(amount, refunded Amount) Status {
	switch {
	case refunded.Sign() <= 0:
		return StatusPaid
	case refunded.Cmp(amount) >= 0:
		return StatusRefunded
	}
	return StatusPartiallyRefunded
}

// NetCaptured is what the payment contributes to its document's paid total:
// amount - amount_refunded for paid / partially_refunded, zero otherwise.
func (p Payment) NetCaptured() (Amount, error) {
	if !p.Status.Counts() {
		return Amount{}, nil
	}
	amount, err := ParseAmount(p.Amount)
	if err != nil {
		return Amount{}, err
	}
	if p.AmountRefunded == "" {
		return amount, nil
	}
	refunded, err := ParseAmount(p.AmountRefunded)
	if err != nil {
		return Amount{}, err
	}
	return amount.Sub(refunded), nil
}

func oneOf(s string, set []string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}
