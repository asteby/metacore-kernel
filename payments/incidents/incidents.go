// Package incidents is the payments face of the platform incident log
// (package incidents, domain "payments"): a thin facade that fixes the domain
// and speaks payment vocabulary (provider, payment and order references,
// amount). Everything is stored in platform_incidents, so the bell, the
// dashboard widget and the health tab see payment incidents next to every
// other domain's, and the "Pagos sin conciliar" screen is a filter
// (domain=payments) on the generic API.
package incidents

import (
	"context"

	generic "github.com/asteby/metacore-kernel/incidents"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Domain is the incident domain of every row this package writes.
const Domain = generic.DomainPayments

// Incident kinds (the catalogue of docs/DISENO-PAGOS-A-PRUEBA-DE-FALLOS
// §5.6), prefixed by their domain.
const (
	KindSignatureInvalid      = "payments.signature_invalid"
	KindNotificationUnresolve = "payments.notification_unresolved"
	KindDeadLetter            = "payments.dead_letter"
	KindPaymentWriteFailed    = "payments.payment_write_failed"
	KindOrphanPayment         = "payments.orphan_payment"
	KindLatePayment           = "payments.late_payment"
	KindDuplicateCharge       = "payments.duplicate_charge"
	KindAmountMismatch        = "payments.amount_mismatch"
	KindChargeback            = "payments.chargeback"
	KindRefundFailed          = "payments.refund_failed"
	KindSweepFailed           = "payments.sweep_failed"
	KindWebhookSilent         = "payments.webhook_silent"
)

// Severities and statuses are the platform's.
const (
	SeverityHigh   = generic.SeverityHigh
	SeverityMedium = generic.SeverityMedium
	SeverityLow    = generic.SeverityLow

	StatusOpen         = generic.StatusOpen
	StatusAcknowledged = generic.StatusAcknowledged
	StatusResolved     = generic.StatusResolved
	StatusAutoResolved = generic.StatusAutoResolved
)

// EventOpened is the host event published (as "kernel") for a payment incident.
var EventOpened = generic.EventFor(Domain)

// Aliases so callers need not import both packages.
type (
	Incident = generic.Incident
	Config   = generic.Config
)

// Input describes a payment problem to record.
type Input struct {
	Org uuid.UUID
	// Provider is the payment provider's addon key.
	Provider string
	Kind     string
	// Severity defaults to alta.
	Severity string
	// Fingerprint defaults to "<kind>:<provider>".
	Fingerprint string
	PaymentRef  string
	OrderRef    string
	Amount      string
	Currency    string
	Title       string
	ActionHint  string
	Detail      string
	// NotificationID links the inbox row that tripped it (optional).
	NotificationID *uuid.UUID
}

// Filter selects payment incidents.
type Filter struct {
	Org      uuid.UUID
	Provider string
	Kind     string
	OpenOnly bool
	Status   string
	Limit    int
	Offset   int
}

// Store is the payments facade over the generic store.
type Store struct{ g *generic.Store }

// New migrates the shared table and builds the facade.
func New(ctx context.Context, db *gorm.DB, cfg Config) (*Store, error) {
	g, err := generic.New(ctx, db, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{g: g}, nil
}

// NewStore builds the facade without touching the schema.
func NewStore(db *gorm.DB, cfg Config) *Store { return &Store{g: generic.NewStore(db, cfg)} }

// FromGeneric wraps an existing generic store (one store per host).
func FromGeneric(g *generic.Store) *Store { return &Store{g: g} }

// Generic returns the underlying platform store.
func (s *Store) Generic() *generic.Store { return s.g }

// Open records a payment incident (see generic Store.Open).
func (s *Store) Open(ctx context.Context, in Input) (Incident, bool, error) {
	meta := map[string]string{}
	for k, v := range map[string]string{"order_ref": in.OrderRef, "amount": in.Amount, "currency": in.Currency} {
		if v != "" {
			meta[k] = v
		}
	}
	g := generic.Input{
		Org: in.Org, Domain: Domain, Source: in.Provider, Kind: in.Kind, Severity: in.Severity,
		Fingerprint: in.Fingerprint, ResourceType: "payment", ResourceID: in.PaymentRef, Meta: meta,
		Title: in.Title, ActionHint: in.ActionHint, Detail: in.Detail,
	}
	if in.PaymentRef == "" {
		g.ResourceType = ""
	}
	if in.NotificationID != nil {
		g.EvidenceID = in.NotificationID.String()
	}
	return s.g.Open(ctx, g)
}

// CloseByFingerprint auto-closes the open payment incident of a fingerprint.
func (s *Store) CloseByFingerprint(ctx context.Context, org uuid.UUID, fingerprint, resolution string) (int64, error) {
	return s.g.CloseByFingerprint(ctx, org, Domain, fingerprint, resolution)
}

// Acknowledge marks an incident as seen.
func (s *Store) Acknowledge(ctx context.Context, org, id uuid.UUID, by string) error {
	return s.g.Acknowledge(ctx, org, id, by)
}

// Resolve closes an incident by a person's decision; a reason is mandatory.
func (s *Store) Resolve(ctx context.Context, org, id uuid.UUID, by, resolution string) error {
	return s.g.Resolve(ctx, org, id, by, resolution)
}

// Get returns one payment incident.
func (s *Store) Get(ctx context.Context, org, id uuid.UUID) (Incident, error) {
	inc, err := s.g.Get(ctx, org, id)
	if err == nil && inc.Domain != Domain {
		return Incident{}, gorm.ErrRecordNotFound
	}
	return inc, err
}

// List returns payment incidents, highest severity first.
func (s *Store) List(ctx context.Context, f Filter) ([]Incident, int64, error) {
	return s.g.List(ctx, generic.Filter{Org: f.Org, Domain: Domain, Source: f.Provider, Kind: f.Kind,
		OpenOnly: f.OpenOnly, Status: f.Status, Limit: f.Limit, Offset: f.Offset})
}

// OpenCounts returns open payment incidents per severity.
func (s *Store) OpenCounts(ctx context.Context, org uuid.UUID) (map[string]int64, error) {
	return s.g.OpenCounts(ctx, org, Domain)
}
