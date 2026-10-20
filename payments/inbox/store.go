package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/payments/incidents"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RetryKind is the outbox job kind hosts use to re-run a notification.
const RetryKind = "payment_notification"

// DefaultBackoff are the waits after the 1st..9th failed attempt (about 45
// hours in all); one more failure dead-letters the notification.
var DefaultBackoff = []time.Duration{
	10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute,
	2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// Default retention (decision D7).
const (
	DefaultRetainRaw    = 90 * 24 * time.Hour
	DefaultRetainResult = 24 * 30 * 24 * time.Hour
	DefaultDedupeWindow = time.Hour
	// DefaultStaleAfter is how long a row may sit in recibida/procesando
	// before it counts as orphaned by a crashed process.
	DefaultStaleAfter = 30 * time.Second
)

// ErrNotFound is returned when a notification does not exist for the org.
var ErrNotFound = errors.New("inbox: notification not found")

// Config configures a Store. Every field is optional.
type Config struct {
	// Incidents opens the incidents the outcome calls for. Nil disables them.
	Incidents *incidents.Store
	// Backoff replaces DefaultBackoff.
	Backoff []time.Duration
	// Jitter is the +/- fraction applied to a backoff step (default 0.2;
	// negative disables it).
	Jitter float64
	// OnRetry is called after a notification is scheduled for retry, so the
	// host can enqueue a worker job at n.ProximoIntentoAt.
	OnRetry func(ctx context.Context, n Notification)
	// DedupeWindow is how far back an identical request counts as duplicate.
	DedupeWindow time.Duration
	RetainRaw    time.Duration
	RetainResult time.Duration
	Logger       *slog.Logger
	Now          func() time.Time
}

// Store is the journal.
type Store struct {
	db  *gorm.DB
	cfg Config
	log *slog.Logger
	pg  bool
}

// New builds a Store and migrates its schema.
func New(ctx context.Context, db *gorm.DB, cfg Config) (*Store, error) {
	if err := Migrate(ctx, db); err != nil {
		return nil, err
	}
	return NewStore(db, cfg), nil
}

// NewStore builds a Store without touching the schema.
func NewStore(db *gorm.DB, cfg Config) *Store {
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.Jitter == 0 {
		cfg.Jitter = 0.2
	}
	if cfg.DedupeWindow <= 0 {
		cfg.DedupeWindow = DefaultDedupeWindow
	}
	if cfg.RetainRaw <= 0 {
		cfg.RetainRaw = DefaultRetainRaw
	}
	if cfg.RetainResult <= 0 {
		cfg.RetainResult = DefaultRetainResult
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Store{db: db, cfg: cfg, log: cfg.Logger.With("component", "payments.inbox"), pg: db.Dialector.Name() == "postgres"}
}

func (s *Store) now() time.Time { return s.cfg.Now().UTC() }

// DedupeKey is the identity of a request for deduplication: provider, route,
// the canonical query and the SHA-256 of the whole body.
func DedupeKey(provider, route string, query map[string][]string, bodySHA string) string {
	q, _ := json.Marshal(query) // map keys are sorted
	sum := sha256.Sum256([]byte(provider + "|" + route + "|" + string(q) + "|" + bodySHA))
	return hex.EncodeToString(sum[:])
}

type insertRow struct {
	ID             uuid.UUID  `gorm:"column:id"`
	OrganizationID uuid.UUID  `gorm:"column:organization_id"`
	Provider       string     `gorm:"column:provider"`
	Route          string     `gorm:"column:route"`
	ReceivedAt     time.Time  `gorm:"column:received_at"`
	SourceIP       string     `gorm:"column:source_ip"`
	Method         string     `gorm:"column:method"`
	Query          string     `gorm:"column:query"`
	Headers        string     `gorm:"column:headers"`
	BodyRaw        []byte     `gorm:"column:body_raw"`
	BodySHA256     string     `gorm:"column:body_sha256"`
	BodyTruncated  bool       `gorm:"column:body_truncated"`
	BodyRedacted   string     `gorm:"column:body_redacted"`
	Auth           string     `gorm:"column:auth"`
	Estado         string     `gorm:"column:estado"`
	Motivo         string     `gorm:"column:motivo"`
	ProcesadaAt    *time.Time `gorm:"column:procesada_at"`
	ReplayOf       *uuid.UUID `gorm:"column:replay_of"`
	DedupeKey      string     `gorm:"column:dedupe_key"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func (insertRow) TableName() string { return TableNotifications }

// Record persists an inbound request BEFORE anything decides about it. If it
// returns an error nothing was stored and the host must answer 503. An
// identical request already handled inside the dedupe window is still stored
// (the journal is complete) but comes back with Estado "duplicada": the host
// answers 200 and skips processing.
func (s *Store) Record(ctx context.Context, req Request) (Notification, error) {
	if req.Org == uuid.Nil {
		return Notification{}, errors.New("inbox: organization is required")
	}
	if strings.TrimSpace(req.Provider) == "" {
		return Notification{}, errors.New("inbox: provider is required")
	}
	received := req.ReceivedAt
	if received.IsZero() {
		received = s.now()
	}
	received = received.UTC()
	sum := sha256.Sum256(req.Body)
	bodySHA := hex.EncodeToString(sum[:])
	raw := req.Body
	truncated := false
	if len(raw) > MaxBodyBytes {
		raw, truncated = raw[:MaxBodyBytes], true
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodPost
	}
	query := map[string][]string{}
	for k, v := range req.Query {
		query[k] = append([]string(nil), v...)
	}
	qJSON, _ := json.Marshal(query)
	hJSON, _ := json.Marshal(RedactHeaders(req.Headers))
	// Redact the bounded copy: a truncated body is not valid JSON.
	redacted := RedactBody(raw)
	id, err := uuid.NewV7()
	if err != nil {
		return Notification{}, err
	}
	row := insertRow{
		ID: id, OrganizationID: req.Org, Provider: req.Provider, Route: req.Route, ReceivedAt: received,
		SourceIP: validIP(req.SourceIP), Method: method, Query: string(qJSON), Headers: string(hJSON),
		BodyRaw: append([]byte(nil), raw...), BodySHA256: bodySHA, BodyTruncated: truncated,
		BodyRedacted: string(redacted), Auth: AuthPendiente, Estado: EstadoRecibida,
		DedupeKey: DedupeKey(req.Provider, req.Route, query, bodySHA), UpdatedAt: received,
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if s.pg {
			// Serialise twins so exactly one of two simultaneous deliveries
			// is the original.
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockKey(req.Org.String()+row.DedupeKey)).Error; err != nil {
				return err
			}
		}
		dup, err := s.hasTwin(tx, row, received)
		if err != nil {
			return err
		}
		if dup {
			row.Estado = EstadoDuplicada
			row.Motivo = "misma notificación ya recibida"
			row.ProcesadaAt = &received
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return Notification{}, fmt.Errorf("inbox: record: %w", err)
	}
	return s.Get(ctx, req.Org, id)
}

// hasTwin reports an identical earlier request that was handled (or is being
// handled right now). A twin that failed or awaits review does not count: a
// provider's retry of a failed delivery must be processed.
func (s *Store) hasTwin(tx *gorm.DB, row insertRow, now time.Time) (bool, error) {
	var n int64
	err := tx.Table(TableNotifications).
		Where("organization_id = ? AND dedupe_key = ? AND received_at >= ?", row.OrganizationID, row.DedupeKey, now.Add(-s.cfg.DedupeWindow)).
		Where("(estado IN ? OR (estado IN ? AND received_at >= ?))",
			[]string{EstadoProcesada, EstadoReconciliadaAPI, EstadoIgnorada, EstadoDuplicada, EstadoResueltaManual},
			[]string{EstadoRecibida, EstadoProcesando}, now.Add(-DefaultStaleAfter)).
		Count(&n).Error
	return n > 0, err
}

// Get returns a notification (without body_raw).
func (s *Store) Get(ctx context.Context, org, id uuid.UUID) (Notification, error) {
	var n Notification
	res := s.db.WithContext(ctx).Table(TableNotifications).Select(notificationColumns).
		Where("organization_id = ? AND id = ?", org, id).Limit(1).Find(&n)
	if res.Error != nil {
		return Notification{}, res.Error
	}
	if res.RowsAffected == 0 {
		return Notification{}, ErrNotFound
	}
	return n, nil
}

// Raw returns the stored request body and whether it was truncated. Callers
// gate it behind payments.notifications.raw and audit the access. Nil after
// the retention purge.
func (s *Store) Raw(ctx context.Context, org, id uuid.UUID) (body []byte, truncated bool, err error) {
	var r struct {
		BodyRaw       []byte `gorm:"column:body_raw"`
		BodyTruncated bool   `gorm:"column:body_truncated"`
	}
	res := s.db.WithContext(ctx).Table(TableNotifications).Select("body_raw, body_truncated").
		Where("organization_id = ? AND id = ?", org, id).Limit(1).Find(&r)
	if res.Error != nil {
		return nil, false, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, false, ErrNotFound
	}
	return r.BodyRaw, r.BodyTruncated, nil
}

// Attempts returns the attempt history of a notification, oldest first.
func (s *Store) Attempts(ctx context.Context, org, id uuid.UUID) ([]Attempt, error) {
	var out []Attempt
	err := s.db.WithContext(ctx).Where("organization_id = ? AND notification_id = ?", org, id).Order("n").Find(&out).Error
	return out, err
}

// Claim marks a notification as being processed and opens its next attempt.
// ok is false when it is not claimable: terminal, or another worker holds it
// (a procesando row older than DefaultStaleAfter is claimable again). A
// manual rerun passes force=true to also reclaim requiere_revision,
// dead_letter and already finished rows.
func (s *Store) Claim(ctx context.Context, org, id uuid.UUID, actor string, force bool) (n Notification, attempt int, ok bool, err error) {
	if actor == "" {
		actor = "system"
	}
	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur Notification
		q := tx.Table(TableNotifications).Select(notificationColumns).Where("organization_id = ? AND id = ?", org, id)
		if s.pg {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		res := q.Limit(1).Find(&cur)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		claimable := false
		switch cur.Estado {
		case EstadoRecibida, EstadoReintentar:
			claimable = true
		case EstadoProcesando:
			claimable = cur.UpdatedAt.Before(now.Add(-DefaultStaleAfter))
		default:
			claimable = force
		}
		if !claimable {
			n = cur
			return nil
		}
		attempt = cur.Intentos + 1
		if err := tx.Table(TableNotifications).Where("id = ?", id).
			Updates(map[string]any{"estado": EstadoProcesando, "updated_at": now, "proximo_intento_at": nil}).Error; err != nil {
			return err
		}
		if err := upsertAttemptStart(tx, org, id, attempt, now, actor); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return Notification{}, 0, false, err
	}
	if ok {
		n, err = s.Get(ctx, org, id)
		return n, attempt, ok, err
	}
	return n, 0, false, nil
}

func upsertAttemptStart(tx *gorm.DB, org, id uuid.UUID, n int, at time.Time, actor string) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Attempt{
		NotificationID: id, N: n, OrganizationID: org, StartedAt: at, Resultado: EstadoProcesando, Actor: actor,
	}).Error
}

// Resolve stores the outcome of an attempt: it opens or closes the incidents
// the outcome calls for, updates the result columns, closes the attempt row
// and, for Estado=reintentar, schedules the next try (or dead-letters the
// notification when the backoff is exhausted). A failed incident write makes
// Resolve fail BEFORE the row changes, so the notification stays recibida and
// the orphan sweep retries it: an outcome that needs a person is never
// recorded without that person's incident.
func (s *Store) Resolve(ctx context.Context, org, id uuid.UUID, r Result) (Notification, error) {
	if r.Estado == "" {
		return Notification{}, errors.New("inbox: Resolve needs an estado")
	}
	cur, err := s.Get(ctx, org, id)
	if err != nil {
		return Notification{}, err
	}
	now := s.now()
	attempt := cur.Intentos + 1
	var next *time.Time
	if r.Estado == EstadoReintentar {
		if attempt > len(s.cfg.Backoff) {
			r.Estado = EstadoDeadLetter
			r.Motivo = strings.TrimSpace("reintentos agotados: " + r.Motivo)
		} else {
			d := r.RetryAfter
			if d <= 0 {
				d = s.backoff(attempt)
			}
			t := now.Add(d)
			next = &t
		}
	}
	if r.Actor == "" {
		r.Actor = "system"
	}
	merged := mergeIdentity(cur, r)
	incidentID, err := s.applyIncidents(ctx, cur, merged, r)
	if err != nil {
		return Notification{}, err
	}

	upd := map[string]any{
		"estado": r.Estado, "motivo": truncate(r.Motivo, 500), "intentos": attempt, "updated_at": now,
		"proximo_intento_at": next,
	}
	for col, v := range map[string]string{
		"auth": r.Auth, "auth_reason": r.AuthReason, "resource_kind": r.ResourceKind,
		"resource_id": r.ResourceID, "event_id": r.EventID, "checkout_ref": r.CheckoutRef,
	} {
		if v != "" {
			upd[col] = v
		}
	}
	switch r.Estado {
	case EstadoReintentar, EstadoProcesando, EstadoRecibida:
	default:
		upd["procesada_at"] = now
	}
	if r.PaymentID != nil {
		upd["payment_id"] = *r.PaymentID
	}
	if incidentID != nil {
		upd["incident_id"] = *incidentID
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table(TableNotifications).Where("organization_id = ? AND id = ?", org, id).Updates(upd).Error; err != nil {
			return err
		}
		if err := upsertAttemptStart(tx, org, id, attempt, cur.ReceivedAt, r.Actor); err != nil {
			return err
		}
		return tx.Table(TableAttempts).Where("notification_id = ? AND n = ?", id, attempt).
			Updates(map[string]any{"ended_at": now, "resultado": r.Estado, "motivo": truncate(r.Motivo, 500), "http_status": r.HTTPStatus}).Error
	})
	if err != nil {
		return Notification{}, fmt.Errorf("inbox: resolve: %w", err)
	}
	out, err := s.Get(ctx, org, id)
	if err != nil {
		return Notification{}, err
	}
	if out.Estado == EstadoReintentar && s.cfg.OnRetry != nil {
		s.cfg.OnRetry(ctx, out)
	}
	return out, nil
}

// ResolveManual closes a requiere_revision or dead_letter notification by a
// person's decision (a reason is mandatory; the host audits it).
func (s *Store) ResolveManual(ctx context.Context, org, id uuid.UUID, by, reason string) (Notification, error) {
	if strings.TrimSpace(reason) == "" {
		return Notification{}, errors.New("inbox: a reason is required")
	}
	cur, err := s.Get(ctx, org, id)
	if err != nil {
		return Notification{}, err
	}
	if cur.Estado != EstadoRequiereRevision && cur.Estado != EstadoDeadLetter {
		return Notification{}, fmt.Errorf("inbox: %s cannot be resolved manually", cur.Estado)
	}
	return s.Resolve(ctx, org, id, Result{Estado: EstadoResueltaManual, Motivo: reason, Actor: by})
}

func (s *Store) backoff(attempt int) time.Duration {
	d := s.cfg.Backoff[attempt-1]
	if s.cfg.Jitter > 0 {
		d = time.Duration(float64(d) * (1 + (rand.Float64()*2-1)*s.cfg.Jitter))
	}
	return d
}

// Due returns notifications scheduled for retry whose time has come, plus
// rows orphaned in recibida/procesando by a crash. Oldest first.
func (s *Store) Due(ctx context.Context, limit int) ([]Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	now := s.now()
	stale := now.Add(-DefaultStaleAfter)
	var out []Notification
	err := s.db.WithContext(ctx).Table(TableNotifications).Select(notificationColumns).
		Where("(estado = ? AND proximo_intento_at <= ?) OR (estado IN ? AND updated_at < ?)",
			EstadoReintentar, now, []string{EstadoRecibida, EstadoProcesando}, stale).
		Order("received_at").Limit(limit).Find(&out).Error
	return out, err
}

// Filter selects notifications for List and Export.
type Filter struct {
	Org        uuid.UUID
	Provider   string
	Estados    []string
	Auth       string
	ResourceID string
	From, To   time.Time
	Limit      int
	Offset     int
}

func (s *Store) filtered(ctx context.Context, f Filter) *gorm.DB {
	q := s.db.WithContext(ctx).Table(TableNotifications).Where("organization_id = ?", f.Org)
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
	if len(f.Estados) > 0 {
		q = q.Where("estado IN ?", f.Estados)
	}
	if f.Auth != "" {
		q = q.Where("auth = ?", f.Auth)
	}
	if f.ResourceID != "" {
		q = q.Where("resource_id = ?", f.ResourceID)
	}
	if !f.From.IsZero() {
		q = q.Where("received_at >= ?", f.From.UTC())
	}
	if !f.To.IsZero() {
		q = q.Where("received_at < ?", f.To.UTC())
	}
	return q
}

// List returns notifications newest first and the total count.
func (s *Store) List(ctx context.Context, f Filter) ([]Notification, int64, error) {
	var total int64
	if err := s.filtered(ctx, f).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Notification
	err := s.filtered(ctx, f).Select(notificationColumns).Order("received_at DESC, id DESC").Limit(limit).Offset(f.Offset).Find(&out).Error
	return out, total, err
}

// Stats summarises an org's journal for banners and the health report.
type Stats struct {
	// ByEstado counts rows in each open estado.
	ByEstado map[string]int64 `json:"by_estado"`
	// InvalidSignature24h counts firma_invalida rows of the last 24 h per provider.
	InvalidSignature24h map[string]int64 `json:"invalid_signature_24h"`
	// LastSigned is the last firma_ok notification per provider.
	LastSigned map[string]time.Time `json:"last_signed"`
}

// Stats computes Stats for an org.
func (s *Store) Stats(ctx context.Context, org uuid.UUID) (Stats, error) {
	st := Stats{ByEstado: map[string]int64{}, InvalidSignature24h: map[string]int64{}, LastSigned: map[string]time.Time{}}
	var byEstado []struct {
		Estado string
		N      int64
	}
	if err := s.db.WithContext(ctx).Table(TableNotifications).Select("estado, COUNT(*) AS n").
		Where("organization_id = ? AND estado IN ?", org, OpenEstados).Group("estado").Scan(&byEstado).Error; err != nil {
		return st, err
	}
	for _, r := range byEstado {
		st.ByEstado[r.Estado] = r.N
	}
	var inv []struct {
		Provider string
		N        int64
	}
	if err := s.db.WithContext(ctx).Table(TableNotifications).Select("provider, COUNT(*) AS n").
		Where("organization_id = ? AND auth = ? AND received_at >= ?", org, AuthFirmaInvalida, s.now().Add(-24*time.Hour)).
		Group("provider").Scan(&inv).Error; err != nil {
		return st, err
	}
	for _, r := range inv {
		st.InvalidSignature24h[r.Provider] = r.N
	}
	var ok []struct {
		Provider string
		At       string
	}
	if err := s.db.WithContext(ctx).Table(TableNotifications).Select("provider, MAX(received_at) AS at").
		Where("organization_id = ? AND auth = ?", org, AuthFirmaOK).Group("provider").Scan(&ok).Error; err != nil {
		return st, err
	}
	for _, r := range ok {
		if t, ok := parseTime(r.At); ok {
			st.LastSigned[r.Provider] = t
		}
	}
	return st, nil
}

func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func validIP(s string) string {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String()
	}
	return ""
}

// SourceIP picks the client IP for the journal. remoteAddr is the TCP peer;
// the forwarded headers (CF-Connecting-IP, then X-Real-IP) are believed only
// when trusted(peer) says the peer is the host's own proxy or edge. A nil
// trusted function trusts loopback and private peers (nginx on the box).
func SourceIP(remoteAddr string, h http.Header, trusted func(netip.Addr) bool) string {
	peer := validIP(remoteAddr)
	if peer == "" {
		return ""
	}
	if trusted == nil {
		trusted = func(a netip.Addr) bool { return a.IsLoopback() || a.IsPrivate() }
	}
	if a, err := netip.ParseAddr(peer); err == nil && trusted(a) {
		for _, name := range []string{"CF-Connecting-IP", "X-Real-IP"} {
			if v := validIP(h.Get(name)); v != "" {
				return v
			}
		}
	}
	return peer
}
