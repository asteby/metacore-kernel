package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
)

type paymentRow struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	OrganizationID uuid.UUID  `gorm:"column:organization_id"`
	DocumentType   string     `gorm:"column:document_type"`
	DocumentID     string     `gorm:"column:document_id"`
	OrderRef       string     `gorm:"column:order_ref"`
	Channel        string     `gorm:"column:channel"`
	Provider       string     `gorm:"column:provider"`
	ProviderRef    *string    `gorm:"column:provider_ref"`
	SessionID      string     `gorm:"column:session_id"`
	DeviceID       string     `gorm:"column:device_id"`
	IdempotencyKey string     `gorm:"column:idempotency_key"`
	Status         string     `gorm:"column:status"`
	Amount         string     `gorm:"column:amount"`
	Currency       string     `gorm:"column:currency"`
	DocumentTotal  *string    `gorm:"column:document_total"`
	AmountRefunded string     `gorm:"column:amount_refunded"`
	Fee            *string    `gorm:"column:fee"`
	Net            *string    `gorm:"column:net"`
	Method         string     `gorm:"column:method"`
	Installments   int        `gorm:"column:installments"`
	Card           *string    `gorm:"column:card"`
	Cash           *string    `gorm:"column:cash"`
	Seller         *string    `gorm:"column:seller"`
	TestMode       bool       `gorm:"column:test_mode"`
	StatusDetail   string     `gorm:"column:status_detail"`
	Anomaly        string     `gorm:"column:anomaly"`
	AnomalyStatus  string     `gorm:"column:anomaly_status"`
	CreatedBy      string     `gorm:"column:created_by"`
	ExpiresAt      *time.Time `gorm:"column:expires_at"`
	LastCheckedAt  *time.Time `gorm:"column:last_checked_at"`
	PaidAt         *time.Time `gorm:"column:paid_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func (paymentRow) TableName() string { return TablePayments }

type eventRow struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	OrganizationID uuid.UUID  `gorm:"column:organization_id"`
	PaymentID      *uuid.UUID `gorm:"column:payment_id"`
	Provider       string     `gorm:"column:provider"`
	EventID        string     `gorm:"column:event_id"`
	Kind           string     `gorm:"column:kind"`
	Source         string     `gorm:"column:source"`
	Status         string     `gorm:"column:status"`
	Decision       string     `gorm:"column:decision"`
	Reason         string     `gorm:"column:reason"`
	Outcome        *string    `gorm:"column:outcome"`
	ObservedAt     time.Time  `gorm:"column:observed_at"`
}

func (eventRow) TableName() string { return TableEvents }

type refundRow struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	OrganizationID     uuid.UUID  `gorm:"column:organization_id"`
	PaymentID          uuid.UUID  `gorm:"column:payment_id"`
	Destination        string     `gorm:"column:destination"`
	Provider           string     `gorm:"column:provider"`
	ProviderRefundID   string     `gorm:"column:provider_refund_id"`
	Amount             string     `gorm:"column:amount"`
	Currency           string     `gorm:"column:currency"`
	Status             string     `gorm:"column:status"`
	Reason             string     `gorm:"column:reason"`
	IdempotencyKey     string     `gorm:"column:idempotency_key"`
	SourceType         string     `gorm:"column:source_type"`
	SourceID           string     `gorm:"column:source_id"`
	CreditNoteRequired bool       `gorm:"column:credit_note_required"`
	AdjustsTotal       bool       `gorm:"column:adjusts_total"`
	CreditNoteID       string     `gorm:"column:credit_note_id"`
	CashSessionID      string     `gorm:"column:cash_session_id"`
	Instructions       string     `gorm:"column:instructions"`
	CreatedBy          string     `gorm:"column:created_by"`
	SettledAt          *time.Time `gorm:"column:settled_at"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

func (refundRow) TableName() string { return TableRefunds }

type holdRow struct {
	ID             uuid.UUID `gorm:"column:id;primaryKey"`
	OrganizationID uuid.UUID `gorm:"column:organization_id"`
	DocumentType   string    `gorm:"column:document_type"`
	DocumentID     string    `gorm:"column:document_id"`
	TokenHash      string    `gorm:"column:token_hash"`
	Holder         string    `gorm:"column:holder"`
	Channel        string    `gorm:"column:channel"`
	UserID         string    `gorm:"column:user_id"`
	Status         string    `gorm:"column:status"`
	CreatedBy      string    `gorm:"column:created_by"`
	ExpiresAt      time.Time `gorm:"column:expires_at"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (holdRow) TableName() string { return TableHolds }

// hashToken is what the holds table stores: the token itself never lands in
// the database, so a database reader cannot charge under someone's hold.
func hashToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("metacore-payments-hold:" + token))
	return hex.EncodeToString(sum[:])
}

// norm renders a stored decimal in the wire form ("1499.000000" → "1499.00").
func norm(s string) string {
	if s == "" {
		return ""
	}
	a, err := payments.ParseAmount(s)
	if err != nil {
		return s
	}
	return a.String()
}

func normPtr(s *string) string {
	if s == nil {
		return ""
	}
	return norm(*s)
}

func jsonPtr(v any) *string {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	s := string(b)
	return &s
}

func decodeInto[T any](s *string) *T {
	if s == nil || *s == "" {
		return nil
	}
	var v T
	if json.Unmarshal([]byte(*s), &v) != nil {
		return nil
	}
	return &v
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (r paymentRow) payment() payments.Payment {
	p := payments.Payment{
		ID: r.ID.String(), DocumentType: r.DocumentType, DocumentID: r.DocumentID, OrderRef: r.OrderRef,
		Channel: payments.Channel(r.Channel), Provider: r.Provider, SessionID: r.SessionID, DeviceID: r.DeviceID,
		IdempotencyKey: r.IdempotencyKey, Status: payments.Status(r.Status),
		Amount: norm(r.Amount), Currency: r.Currency, AmountRefunded: norm(r.AmountRefunded),
		Fee: normPtr(r.Fee), Net: normPtr(r.Net), Method: r.Method, Installments: r.Installments,
		Card: decodeInto[payments.Card](r.Card), Cash: decodeInto[payments.CashTender](r.Cash),
		Seller: decodeInto[payments.Seller](r.Seller), TestMode: r.TestMode, Anomaly: r.Anomaly,
		PaidAt: r.PaidAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ProviderRef != nil {
		p.ProviderRef = *r.ProviderRef
	}
	if p.AmountRefunded == "0.00" {
		p.AmountRefunded = ""
	}
	return p
}

func (r refundRow) refund() payments.Refund {
	return payments.Refund{
		ID: r.ID.String(), PaymentID: r.PaymentID.String(), Destination: payments.RefundDestination(r.Destination),
		Provider: r.Provider, ProviderRefundID: r.ProviderRefundID, Amount: norm(r.Amount), Currency: r.Currency,
		Status: payments.RefundStatus(r.Status), Reason: r.Reason, IdempotencyKey: r.IdempotencyKey,
		SourceType: r.SourceType, SourceID: r.SourceID, CreditNoteRequired: r.CreditNoteRequired,
		AdjustsTotal: r.AdjustsTotal, CreditNoteID: r.CreditNoteID, CashSessionID: r.CashSessionID, CreatedAt: r.CreatedAt, SettledAt: r.SettledAt,
	}
}

func (r holdRow) hold() payments.Hold {
	return payments.Hold{
		DocumentType: r.DocumentType, DocumentID: r.DocumentID, Token: r.TokenHash, Holder: r.Holder,
		Channel: payments.Channel(r.Channel), UserID: r.UserID, Status: payments.HoldStatus(r.Status), ExpiresAt: r.ExpiresAt,
	}
}
