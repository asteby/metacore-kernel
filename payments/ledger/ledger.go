// Package ledger is the payments ledger (payments standard, phase K2): the
// single writer of the canonical Payment, Refund, PaymentEvent and Hold
// records (docs/RFC-PAYMENTS-STANDARD.md). Channels (POS, cash desk, kiosk,
// storefront) and provider addons never write these tables: channels charge,
// cancel and refund through the ledger, which calls the provider's
// commerce.payments.* capability itself and records the answer; providers
// report what their webhooks observed through Observe, and only about their
// own payments. Every status the ledger stores was re-read from the provider
// and classified by payments.Observe; every applied change publishes the
// neutral commerce.payments.confirmed event under the provider's key.
//
// Guests reach the ledger through host-implemented capability contracts
// (payments.*, see host.go) over the existing capability_query import, gated
// by the manifest's `capability:query` grants. The reconciliation job
// (reconcile.go, phase K3) re-reads open payments, resolves anomalies and
// finds orphans.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Publisher publishes a host event. events.Bus.Publish has this shape: the
// ledger publishes commerce.payments.confirmed with the provider's addon key
// as the emitter, so subscribers keep checking emitter == provider.
type Publisher func(ctx context.Context, emitter, event string, org uuid.UUID, payload any) error

// Anomaly is a payment the lifecycle does not explain, raised to people
// (the host routes it to the review queue and notifications).
type Anomaly struct {
	Org          uuid.UUID       `json:"organization_id"`
	PaymentID    string          `json:"payment_id,omitempty"`
	DocumentType string          `json:"document_type,omitempty"`
	DocumentID   string          `json:"document_id,omitempty"`
	Provider     string          `json:"provider"`
	ProviderRef  string          `json:"provider_ref,omitempty"`
	OrderRef     string          `json:"order_ref,omitempty"`
	Reason       string          `json:"reason"`
	Stored       payments.Status `json:"stored_status,omitempty"`
	Observed     payments.Status `json:"observed_status,omitempty"`
	Amount       string          `json:"amount,omitempty"`
	Currency     string          `json:"currency,omitempty"`
	Source       payments.Source `json:"source"`
	// Resolved is set when a second consistent read applied the provider's
	// status as a correction (the anomaly is closed, still worth a look).
	Resolved bool `json:"resolved,omitempty"`
}

// Anomaly reasons beyond payments.Observe's.
const (
	// ReasonOrphan: the provider reports a payment the ledger does not know
	// (money without a document).
	ReasonOrphan = "orphan"
	// ReasonAmountMismatch: the provider captured another amount than the
	// payment was created for (payments.ObserveOutcome).
	ReasonAmountMismatch = payments.ReasonAmountMismatch
	// ReasonStuck: an open payment the provider would not resolve past its
	// expiry.
	ReasonStuck = "stuck"
)

// moneyMismatch are anomalies a second consistent read never corrects on
// its own: the re-read is not about this payment's money, a human decides.
var moneyMismatch = map[string]bool{
	payments.ReasonAmountMismatch: true, payments.ReasonCurrencyMismatch: true,
	payments.ReasonProviderRefMismatch: true, payments.ReasonInvalidOutcome: true,
}

// AnomalyEvent is the host event published (as "kernel") for every anomaly,
// so rules and notifications can react without knowing the ledger.
const AnomalyEvent = "kernel.payments.anomaly"

// AnomalySink receives anomalies (review queue + notification in the host).
type AnomalySink interface {
	PaymentAnomaly(ctx context.Context, a Anomaly) error
}

// AnomalySinkFunc adapts a function to AnomalySink.
type AnomalySinkFunc func(ctx context.Context, a Anomaly) error

// PaymentAnomaly implements AnomalySink.
func (f AnomalySinkFunc) PaymentAnomaly(ctx context.Context, a Anomaly) error { return f(ctx, a) }

// Config wires a Ledger.
type Config struct {
	// Providers answers commerce.payments.* queries (the host's provider
	// querier, NOT the HostQuerier wrapping this ledger).
	Providers capability.Querier
	// Publish publishes events; nil = no events.
	Publish Publisher
	// Anomalies receives anomalies; nil = logged by the caller only.
	Anomalies AnomalySink
	// IsPaymentsProvider reports whether addon provides a commerce.payments.*
	// capability in org (Observe from a guest requires it).
	IsPaymentsProvider func(ctx context.Context, org uuid.UUID, addon string) (bool, error)
	// Now is the clock; nil = time.Now.
	Now func() time.Time
	// NewToken returns an unguessable hold token; nil = payments.NewHoldToken.
	NewToken func() string
	// SkipMigrate skips Migrate in New.
	SkipMigrate bool
}

// Ledger is the payments ledger. Safe for concurrent use.
type Ledger struct {
	db  *gorm.DB
	cfg Config
}

// New builds a Ledger and (unless SkipMigrate) creates its tables.
func New(ctx context.Context, db *gorm.DB, cfg Config) (*Ledger, error) {
	if db == nil {
		return nil, errors.New("payments ledger: nil db")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewToken == nil {
		cfg.NewToken = payments.NewHoldToken
	}
	if !cfg.SkipMigrate {
		if err := Migrate(ctx, db); err != nil {
			return nil, err
		}
	}
	return &Ledger{db: db, cfg: cfg}, nil
}

func (l *Ledger) now() time.Time { return l.cfg.Now().UTC() }

// Error codes the ledger answers (stable: channels branch on them).
const (
	CodeInvalidInput      = "invalid_input"
	CodeNotFound          = "not_found"
	CodeInPayment         = "in_payment"
	CodeHoldNotOwner      = "hold_not_owner"
	CodeInvalidTransition = "invalid_transition"
	CodeExceedsDue        = "exceeds_due"
	CodeNotRefundable     = "not_refundable"
	CodeRefundExceeds     = "refund_exceeds"
	CodeIdempotency       = "idempotency_conflict"
	CodeForbidden         = "forbidden"
	CodeProvider          = "provider_error"
	CodeUnavailable       = "unavailable"
)

// Error is a ledger failure with a stable code.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func fail(code string, err error) error { return &Error{Code: code, Err: err} }

func failf(code, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// ErrorCode returns the ledger code of err ("" for nil, "internal" when
// uncoded).
func ErrorCode(err error) string {
	var e *Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &e):
		return e.Code
	}
	return "internal"
}

// lockDocument serializes every write about one document (holds, charges)
// inside tx. Postgres: transaction-scoped advisory lock. SQLite serializes
// writers by itself.
func lockDocument(tx *gorm.DB, org uuid.UUID, docType, docID string) error {
	if !isPostgres(tx) {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", lockKey("doc:"+org.String()+":"+docType+":"+docID)).Error
}

// forUpdate returns a reusable (Session) handle whose reads lock the rows
// on Postgres.
func forUpdate(tx *gorm.DB) *gorm.DB {
	if isPostgres(tx) {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Session(&gorm.Session{})
	}
	return tx.Session(&gorm.Session{})
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// ---------------------------------------------------------------- holds

// HoldResult is a taken / renewed hold. Token is returned in full only here.
type HoldResult struct {
	Hold  payments.Hold
	Token string
}

// AcquireHold takes or renews the charge hold of a document (payments.Acquire
// semantics, atomic per document). A live hold of someone else answers
// in_payment with its holder and expiry.
func (l *Ledger) AcquireHold(ctx context.Context, org uuid.UUID, req payments.HoldRequest, caller string) (HoldResult, error) {
	if blank(req.DocumentType) || blank(req.DocumentID) || blank(req.Holder) {
		return HoldResult{}, failf(CodeInvalidInput, "document_type, document_id and holder are required")
	}
	var out HoldResult
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockDocument(tx, org, req.DocumentType, req.DocumentID); err != nil {
			return err
		}
		cur, err := activeHold(tx, org, req.DocumentType, req.DocumentID)
		if err != nil {
			return err
		}
		now := l.now()
		var raw string
		hreq := req
		hreq.Token = hashToken(req.Token)
		var current *payments.Hold
		if cur != nil {
			h := cur.hold()
			current = &h
		}
		h, err := payments.Acquire(current, hreq, now, func() string { raw = l.cfg.NewToken(); return hashToken(raw) })
		if err != nil {
			if errors.Is(err, payments.ErrHeld) {
				return fail(CodeInPayment, err)
			}
			return fail(CodeInvalidInput, err)
		}
		if raw == "" {
			// Renewal by the owner.
			if err := tx.Model(&holdRow{}).Where("id = ?", cur.ID).
				Updates(map[string]any{"expires_at": h.ExpiresAt, "updated_at": now}).Error; err != nil {
				return err
			}
			h.Token = ""
			out = HoldResult{Hold: h, Token: req.Token}
			return nil
		}
		if cur != nil {
			// Expired (or finished) hold of someone else: close it first so
			// the one-live-hold index admits the new one.
			if err := tx.Model(&holdRow{}).Where("id = ?", cur.ID).
				Updates(map[string]any{"status": string(payments.HoldExpired), "updated_at": now}).Error; err != nil {
				return err
			}
		}
		row := holdRow{
			ID: uuid.New(), OrganizationID: org, DocumentType: req.DocumentType, DocumentID: req.DocumentID,
			TokenHash: h.Token, Holder: req.Holder, Channel: string(req.Channel), UserID: req.UserID,
			Status: string(payments.HoldActive), CreatedBy: caller, ExpiresAt: h.ExpiresAt, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		h.Token = ""
		out = HoldResult{Hold: h, Token: raw}
		return nil
	})
	return out, err
}

// ReleaseHold ends the hold of a document: only its owner, unless force
// (the document is paid). Nothing to release is not an error.
func (l *Ledger) ReleaseHold(ctx context.Context, org uuid.UUID, docType, docID, token string, force bool) error {
	return l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockDocument(tx, org, docType, docID); err != nil {
			return err
		}
		cur, err := activeHold(tx, org, docType, docID)
		if err != nil || cur == nil {
			return err
		}
		h := cur.hold()
		next, err := payments.Release(&h, hashToken(token), force, l.now())
		if err != nil {
			return fail(CodeHoldNotOwner, err)
		}
		return tx.Model(&holdRow{}).Where("id = ?", cur.ID).
			Updates(map[string]any{"status": string(next.Status), "updated_at": l.now()}).Error
	})
}

func activeHold(tx *gorm.DB, org uuid.UUID, docType, docID string) (*holdRow, error) {
	var row holdRow
	err := tx.Where("organization_id = ? AND document_type = ? AND document_id = ? AND status = ?",
		org, docType, docID, string(payments.HoldActive)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ------------------------------------------------------------- documents

// Document is everything the ledger holds about one document.
type Document struct {
	DocumentType string             `json:"document_type"`
	DocumentID   string             `json:"document_id"`
	Payments     []payments.Payment `json:"payments"`
	Refunds      []payments.Refund  `json:"refunds"`
	// NetPaid is the sum of NetCaptured over the payments.
	NetPaid string `json:"net_paid"`
	// Paid is what counts toward the document total (payments.DocumentPaid):
	// captured minus only the refunds that reduced the total. A refund that
	// did not touch the total never reopens the debt.
	Paid string `json:"paid"`
	// Open reports a payment still open (do not charge again).
	Open bool `json:"open"`
	// Hold is the live hold, without its token.
	Hold *payments.Hold `json:"hold,omitempty"`
}

// Document returns the payments, refunds and live hold of a document.
func (l *Ledger) Document(ctx context.Context, org uuid.UUID, docType, docID string) (Document, error) {
	if blank(docType) || blank(docID) {
		return Document{}, failf(CodeInvalidInput, "document_type and document_id are required")
	}
	return l.document(l.db.WithContext(ctx), org, docType, docID)
}

func (l *Ledger) document(tx *gorm.DB, org uuid.UUID, docType, docID string) (Document, error) {
	d := Document{DocumentType: docType, DocumentID: docID, Payments: []payments.Payment{}, Refunds: []payments.Refund{}}
	var rows []paymentRow
	if err := tx.Where("organization_id = ? AND document_type = ? AND document_id = ?", org, docType, docID).
		Order("created_at").Find(&rows).Error; err != nil {
		return d, err
	}
	net := payments.Amount{}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		p := r.payment()
		d.Payments = append(d.Payments, p)
		ids = append(ids, r.ID)
		n, err := p.NetCaptured()
		if err != nil {
			return d, err
		}
		net = net.Add(n)
		d.Open = d.Open || p.Status.Open()
	}
	d.NetPaid = net.String()
	if len(ids) > 0 {
		var refunds []refundRow
		if err := tx.Where("organization_id = ? AND payment_id IN ?", org, ids).Order("created_at").Find(&refunds).Error; err != nil {
			return d, err
		}
		for _, r := range refunds {
			d.Refunds = append(d.Refunds, r.refund())
		}
	}
	paid, err := payments.DocumentPaid(d.Payments, d.Refunds)
	if err != nil {
		return d, err
	}
	d.Paid = paid.String()
	hold, err := activeHold(tx, org, docType, docID)
	if err != nil {
		return d, err
	}
	if hold != nil && hold.ExpiresAt.After(l.now()) {
		h := hold.hold()
		h.Token = ""
		d.Hold = &h
	}
	return d, nil
}

// Payment returns one payment.
func (l *Ledger) Payment(ctx context.Context, org uuid.UUID, id string) (payments.Payment, error) {
	row, err := l.paymentRow(l.db.WithContext(ctx), org, id)
	if err != nil {
		return payments.Payment{}, err
	}
	return row.payment(), nil
}

func (l *Ledger) paymentRow(tx *gorm.DB, org uuid.UUID, id string) (paymentRow, error) {
	pid, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return paymentRow{}, failf(CodeInvalidInput, "payment_id %q is not a uuid", id)
	}
	var row paymentRow
	err = tx.Where("organization_id = ? AND id = ?", org, pid).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, failf(CodeNotFound, "payment %s not found", id)
	}
	return row, err
}
