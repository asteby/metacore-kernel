package incidents

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/notifications"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Domains. The first user is payments; the others are reserved names so every
// producer files under the same vocabulary.
const (
	DomainPayments     = "payments"
	DomainInventory    = "inventory"
	DomainIntegrations = "integrations"
	DomainEvents       = "events"
	DomainUpdates      = "updates"
	DomainStore        = "store"
)

// Severities. "alta" means money or customer data may be at stake.
const (
	SeverityHigh   = "alta"
	SeverityMedium = "media"
	SeverityLow    = "baja"
)

// Statuses.
const (
	StatusOpen         = "abierta"
	StatusAcknowledged = "reconocida"
	StatusResolved     = "resuelta"
	StatusAutoResolved = "auto_resuelta"
)

// EventFor is the host event published (as "kernel") when an incident of the
// domain opens or escalates: kernel.<domain>.incident_opened.
func EventFor(domain string) string { return "kernel." + domain + ".incident_opened" }

// MaxEvidenceIDs bounds Incident.EvidenceIDs (most recent kept).
const MaxEvidenceIDs = 50

// Incident is one row of commerce_payment_incidents.
type Incident struct {
	ID             uuid.UUID `json:"id" gorm:"column:id"`
	OrganizationID uuid.UUID `json:"organization_id" gorm:"column:organization_id"`
	Domain         string    `json:"domain" gorm:"column:domain"`
	// Source is the producer inside the domain (the payment provider, the
	// connector, the addon).
	Source       string     `json:"source,omitempty" gorm:"column:source"`
	Kind         string     `json:"kind" gorm:"column:kind"`
	Severity     string     `json:"severity" gorm:"column:severity"`
	Status       string     `json:"status" gorm:"column:status"`
	Fingerprint  string     `json:"fingerprint" gorm:"column:fingerprint"`
	FirstSeenAt  time.Time  `json:"first_seen_at" gorm:"column:first_seen_at"`
	LastSeenAt   time.Time  `json:"last_seen_at" gorm:"column:last_seen_at"`
	Occurrences  int        `json:"occurrences" gorm:"column:occurrences"`
	ResourceType string     `json:"resource_type,omitempty" gorm:"column:resource_type"`
	ResourceID   string     `json:"resource_id,omitempty" gorm:"column:resource_id"`
	Meta         JSONText   `json:"meta" gorm:"column:meta"`
	Title        string     `json:"title" gorm:"column:title"`
	ActionHint   string     `json:"action_hint,omitempty" gorm:"column:action_hint"`
	Detail       string     `json:"detail,omitempty" gorm:"column:detail"`
	AckedBy      string     `json:"acknowledged_by,omitempty" gorm:"column:acknowledged_by"`
	AckedAt      *time.Time `json:"acknowledged_at,omitempty" gorm:"column:acknowledged_at"`
	ResolvedBy   string     `json:"resolved_by,omitempty" gorm:"column:resolved_by"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty" gorm:"column:resolved_at"`
	Resolution   string     `json:"resolution,omitempty" gorm:"column:resolution"`
	// EvidenceIDs link the records that tripped it (inbox notifications,
	// event deliveries...), most recent MaxEvidenceIDs.
	EvidenceIDs []string `json:"evidence_ids" gorm:"-"`

	EvidenceRaw string    `json:"-" gorm:"column:evidence_ids"`
	CreatedAt   time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt   time.Time `json:"updated_at" gorm:"column:updated_at"`
}

// JSONText is a JSON document in a JSONB (Postgres) or TEXT (SQLite) column.
type JSONText string

// Scan implements sql.Scanner.
func (j *JSONText) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*j = ""
	case string:
		*j = JSONText(t)
	case []byte:
		*j = JSONText(t)
	default:
		return fmt.Errorf("incidents: cannot scan %T into JSONText", v)
	}
	return nil
}

// Value implements driver.Valuer.
func (j JSONText) Value() (driver.Value, error) {
	if j == "" {
		return "{}", nil
	}
	return string(j), nil
}

// MarshalJSON emits the stored document verbatim.
func (j JSONText) MarshalJSON() ([]byte, error) {
	if j == "" || !json.Valid([]byte(j)) {
		return []byte("{}"), nil
	}
	return []byte(j), nil
}

// TableName pins the GORM table.
func (Incident) TableName() string { return TableIncidents }

// IsOpen reports whether the incident still needs attention.
func (i Incident) IsOpen() bool {
	return i.Status == StatusOpen || i.Status == StatusAcknowledged
}

// Input describes a problem to record.
type Input struct {
	Org    uuid.UUID
	Domain string
	// Source is the producer inside the domain (payment provider, connector).
	Source string
	// Kind names the problem, prefixed by its domain ("payments.dead_letter").
	Kind string
	// Severity defaults to alta.
	Severity string
	// Fingerprint groups occurrences. Defaults to "<kind>:<source>".
	Fingerprint  string
	ResourceType string
	ResourceID   string
	// Meta carries domain facts shown by the UI (amount, currency, order...).
	Meta       map[string]string
	Title      string
	ActionHint string
	Detail     string
	// EvidenceID links the record that tripped it (optional).
	EvidenceID string
}

// Publisher publishes a host event; events.Bus.Publish has this shape.
type Publisher func(ctx context.Context, emitter, event string, org uuid.UUID, payload any) error

// Enqueuer is the slice of *notifications.Service the bridge needs.
type Enqueuer interface {
	Enqueue(ctx context.Context, req notifications.EnqueueRequest) (notifications.EnqueueResult, error)
}

// Config wires the bridges. All fields are optional.
type Config struct {
	// Publish emits kernel.payments.incident_opened (emitter "kernel").
	Publish Publisher
	// Notifier receives the in-app notifications.
	Notifier Enqueuer
	// Recipients resolves the targets (user ids) for an org: owners and admins.
	Recipients func(ctx context.Context, org uuid.UUID) ([]string, error)
	// Channel is the notifications channel; default "in_app".
	Channel string
	// RuleFlag mirrors the incident into the host's review queue.
	RuleFlag func(ctx context.Context, inc Incident) error
	Logger   *slog.Logger
	Now      func() time.Time
}

// Store persists and bridges incidents.
type Store struct {
	db  *gorm.DB
	cfg Config
	log *slog.Logger
}

// New builds a Store and creates its table. Use SkipMigrate-style hosts by
// calling NewStore instead.
func New(ctx context.Context, db *gorm.DB, cfg Config) (*Store, error) {
	if err := Migrate(ctx, db); err != nil {
		return nil, err
	}
	return NewStore(db, cfg), nil
}

// NewStore builds a Store without touching the schema.
func NewStore(db *gorm.DB, cfg Config) *Store {
	if cfg.Channel == "" {
		cfg.Channel = "in_app"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Store{db: db, cfg: cfg, log: cfg.Logger.With("component", "payments.incidents")}
}

func (s *Store) now() time.Time { return s.cfg.Now().UTC() }

func sevRank(sev string) int {
	switch sev {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	default:
		return 1
	}
}

var openStatuses = []string{StatusOpen, StatusAcknowledged}

// Open records the problem. A matching open incident (same organization,
// domain and fingerprint) is updated in place (occurrences++, last_seen_at, a
// higher severity wins); otherwise a new one is created. The bridges (event,
// notification, rule flag) fire when the incident is new or escalated.
func (s *Store) Open(ctx context.Context, in Input) (inc Incident, created bool, err error) {
	if in.Org == uuid.Nil {
		return Incident{}, false, errors.New("incidents: organization is required")
	}
	if strings.TrimSpace(in.Domain) == "" {
		return Incident{}, false, errors.New("incidents: domain is required")
	}
	if strings.TrimSpace(in.Kind) == "" {
		return Incident{}, false, errors.New("incidents: kind is required")
	}
	if in.Severity == "" {
		in.Severity = SeverityHigh
	}
	if in.Fingerprint == "" {
		in.Fingerprint = in.Kind + ":" + in.Source
	}
	var escalated bool
	for attempt := 0; attempt < 3; attempt++ {
		inc, created, escalated, err = s.upsert(ctx, in)
		if err == nil || !isUniqueViolation(err) {
			break
		}
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("incidents: open: %w", err)
	}
	if created || escalated {
		s.bridge(ctx, inc)
	}
	return inc, created, nil
}

func (s *Store) upsert(ctx context.Context, in Input) (Incident, bool, bool, error) {
	var (
		out       Incident
		created   bool
		escalated bool
	)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := s.now()
		var cur Incident
		q := tx.Where("organization_id = ? AND domain = ? AND fingerprint = ? AND status IN ?", in.Org, in.Domain, in.Fingerprint, openStatuses)
		if tx.Dialector.Name() == "postgres" {
			q = q.Clauses(forUpdate())
		}
		res := q.Limit(1).Find(&cur)
		if res.Error != nil {
			return res.Error
		}
		meta, _ := json.Marshal(nonNil(in.Meta))
		if res.RowsAffected == 0 {
			ids := []string{}
			if in.EvidenceID != "" {
				ids = append(ids, in.EvidenceID)
			}
			raw, _ := json.Marshal(ids)
			id, _ := uuid.NewV7()
			row := Incident{
				ID: id, OrganizationID: in.Org, Domain: in.Domain, Source: in.Source, Kind: in.Kind,
				Severity: in.Severity, Status: StatusOpen, Fingerprint: in.Fingerprint,
				FirstSeenAt: now, LastSeenAt: now, Occurrences: 1,
				ResourceType: in.ResourceType, ResourceID: in.ResourceID, Meta: JSONText(meta),
				Title: in.Title, ActionHint: in.ActionHint, Detail: in.Detail,
				EvidenceRaw: string(raw), EvidenceIDs: ids, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			out, created = row, true
			return nil
		}
		ids := decodeIDs(cur.EvidenceRaw)
		if in.EvidenceID != "" {
			ids = append(ids, in.EvidenceID)
			if n := len(ids); n > MaxEvidenceIDs {
				ids = ids[n-MaxEvidenceIDs:]
			}
		}
		raw, _ := json.Marshal(ids)
		upd := map[string]any{
			"occurrences":  gorm.Expr("occurrences + 1"),
			"last_seen_at": now,
			"updated_at":   now,
			"evidence_ids": string(raw),
		}
		if sevRank(in.Severity) > sevRank(cur.Severity) {
			upd["severity"] = in.Severity
			escalated = true
		}
		// Fresher facts win; keep the old text when the new input has none.
		for col, v := range map[string]string{"resource_type": in.ResourceType, "resource_id": in.ResourceID,
			"title": in.Title, "action_hint": in.ActionHint, "detail": in.Detail} {
			if v != "" {
				upd[col] = v
			}
		}
		if len(in.Meta) > 0 {
			upd["meta"] = string(meta)
		}
		if err := tx.Table(TableIncidents).Where("id = ?", cur.ID).Updates(upd).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", cur.ID).Take(&out).Error
	})
	if err != nil {
		return Incident{}, false, false, err
	}
	out.EvidenceIDs = decodeIDs(out.EvidenceRaw)
	return out, created, escalated, nil
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// bridge fans a new or escalated incident out to the host. Failures are
// logged; the incident row already exists.
func (s *Store) bridge(ctx context.Context, inc Incident) {
	if s.cfg.Publish != nil {
		if err := s.cfg.Publish(ctx, "kernel", EventFor(inc.Domain), inc.OrganizationID, inc); err != nil {
			s.log.Warn("publish incident_opened failed", "incident", inc.ID, "err", err)
		}
	}
	if s.cfg.Notifier != nil && s.cfg.Recipients != nil {
		targets, err := s.cfg.Recipients(ctx, inc.OrganizationID)
		if err != nil {
			s.log.Warn("resolve incident recipients failed", "incident", inc.ID, "err", err)
		}
		msg := inc.Title
		if msg == "" {
			msg = inc.Kind
		}
		if inc.ActionHint != "" {
			msg += ": " + inc.ActionHint
		}
		src := inc.ID
		for _, t := range targets {
			_, err := s.cfg.Notifier.Enqueue(ctx, notifications.EnqueueRequest{
				OrganizationID: inc.OrganizationID,
				SourceType:     "incident",
				SourceID:       &src,
				SourceName:     inc.Kind,
				Event:          inc.Domain + ".incident." + inc.Kind,
				Channel:        s.cfg.Channel,
				Target:         t,
				Message:        msg,
				ContextRef:     inc.ID.String(),
				DedupKey:       fmt.Sprintf("incident:%s:%s:%s", inc.ID, inc.Severity, t),
			})
			if err != nil {
				s.log.Warn("enqueue incident notification failed", "incident", inc.ID, "err", err)
			}
		}
	}
	if s.cfg.RuleFlag != nil {
		if err := s.cfg.RuleFlag(ctx, inc); err != nil {
			s.log.Warn("rule flag for incident failed", "incident", inc.ID, "err", err)
		}
	}
}

// Acknowledge marks an open incident as seen by a person (abierta ->
// reconocida). It stays open and keeps grouping occurrences.
func (s *Store) Acknowledge(ctx context.Context, org, id uuid.UUID, by string) error {
	now := s.now()
	res := s.db.WithContext(ctx).Table(TableIncidents).
		Where("organization_id = ? AND id = ? AND status = ?", org, id, StatusOpen).
		Updates(map[string]any{"status": StatusAcknowledged, "acknowledged_by": by, "acknowledged_at": now, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Resolve closes an open incident by a person's decision; a reason is
// mandatory (the host audits who and why).
func (s *Store) Resolve(ctx context.Context, org, id uuid.UUID, by, resolution string) error {
	if strings.TrimSpace(resolution) == "" {
		return errors.New("incidents: a resolution reason is required")
	}
	return s.close(ctx, s.db.WithContext(ctx).Table(TableIncidents).Where("organization_id = ? AND id = ?", org, id), StatusResolved, by, resolution, true)
}

// CloseByFingerprint auto-closes the open incident of a fingerprint in a
// domain (the condition went away: the signature matches again, the API
// answered). It reports how many rows closed; none is not an error.
func (s *Store) CloseByFingerprint(ctx context.Context, org uuid.UUID, domain, fingerprint, resolution string) (int64, error) {
	q := s.db.WithContext(ctx).Table(TableIncidents).Where("organization_id = ? AND domain = ? AND fingerprint = ?", org, domain, fingerprint)
	res := s.closeQ(q, StatusAutoResolved, "system", resolution)
	return res.RowsAffected, res.Error
}

func (s *Store) close(_ context.Context, q *gorm.DB, status, by, resolution string, mustMatch bool) error {
	res := s.closeQ(q, status, by, resolution)
	if res.Error != nil {
		return res.Error
	}
	if mustMatch && res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *Store) closeQ(q *gorm.DB, status, by, resolution string) *gorm.DB {
	now := s.now()
	return q.Where("status IN ?", openStatuses).
		Updates(map[string]any{"status": status, "resolved_by": by, "resolved_at": now, "resolution": resolution, "updated_at": now})
}

// Get returns one incident of an org.
func (s *Store) Get(ctx context.Context, org, id uuid.UUID) (Incident, error) {
	var inc Incident
	if err := s.db.WithContext(ctx).Where("organization_id = ? AND id = ?", org, id).Take(&inc).Error; err != nil {
		return Incident{}, err
	}
	inc.EvidenceIDs = decodeIDs(inc.EvidenceRaw)
	return inc, nil
}

// Filter selects incidents for List.
type Filter struct {
	Org uuid.UUID
	// Domain empty means every domain.
	Domain string
	Source string
	Kind   string
	// OpenOnly keeps abierta and reconocida.
	OpenOnly bool
	Status   string
	Limit    int
	Offset   int
}

func (s *Store) filtered(ctx context.Context, f Filter) *gorm.DB {
	q := s.db.WithContext(ctx).Table(TableIncidents).Where("organization_id = ?", f.Org)
	if f.Domain != "" {
		q = q.Where("domain = ?", f.Domain)
	}
	if f.Source != "" {
		q = q.Where("source = ?", f.Source)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.OpenOnly {
		q = q.Where("status IN ?", openStatuses)
	}
	return q
}

// List returns incidents, highest severity first then most recent, and the
// total matching count.
func (s *Store) List(ctx context.Context, f Filter) ([]Incident, int64, error) {
	var total int64
	if err := s.filtered(ctx, f).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []Incident
	err := s.filtered(ctx, f).Order("CASE severity WHEN 'alta' THEN 3 WHEN 'media' THEN 2 ELSE 1 END DESC").
		Order("last_seen_at DESC").Limit(limit).Offset(f.Offset).Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	for i := range rows {
		rows[i].EvidenceIDs = decodeIDs(rows[i].EvidenceRaw)
	}
	return rows, total, nil
}

// OpenCounts returns the number of open incidents per severity, for one
// domain or (domain "") for all of them.
func (s *Store) OpenCounts(ctx context.Context, org uuid.UUID, domain string) (map[string]int64, error) {
	type row struct {
		Severity string
		N        int64
	}
	var rows []row
	err := s.filtered(ctx, Filter{Org: org, Domain: domain, OpenOnly: true}).Select("severity, COUNT(*) AS n").
		Group("severity").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := map[string]int64{SeverityHigh: 0, SeverityMedium: 0, SeverityLow: 0}
	for _, r := range rows {
		out[r.Severity] = r.N
	}
	return out, nil
}

func decodeIDs(raw string) []string {
	ids := []string{}
	_ = json.Unmarshal([]byte(raw), &ids)
	return ids
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key") || strings.Contains(m, "23505")
}
