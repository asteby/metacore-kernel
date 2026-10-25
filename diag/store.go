package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Errores del Store.
var (
	ErrOrgRequired = errors.New("diag: organization is required")
	ErrNotFound    = errors.New("diag: not found")
	ErrBadStatus   = errors.New("diag: invalid status")
)

// Retencion por defecto.
const (
	EventRetention = 14 * 24 * time.Hour
	GroupRetention = 90 * 24 * time.Hour
	// MaxSamplesPerGroup: muestras (eventos) por grupo, ademas de la primera.
	MaxSamplesPerGroup = 20
)

// Group es un fallo agrupado por huella (un "issue").
type Group struct {
	ID           uuid.UUID  `json:"id" gorm:"column:id;primaryKey"`
	OrgID        uuid.UUID  `json:"org_id" gorm:"column:org_id"`
	AddonKey     string     `json:"addon_key" gorm:"column:addon_key"`
	AddonVersion string     `json:"addon_version,omitempty" gorm:"column:addon_version"`
	Operation    string     `json:"operation,omitempty" gorm:"column:operation"`
	Kind         string     `json:"kind" gorm:"column:kind"`
	Code         string     `json:"code" gorm:"column:code"`
	Severity     string     `json:"severity" gorm:"column:severity"`
	HTTPStatus   int        `json:"http_status,omitempty" gorm:"column:http_status"`
	Fingerprint  string     `json:"fingerprint" gorm:"column:fingerprint"`
	Count        int64      `json:"count" gorm:"column:count"`
	FirstSeenAt  time.Time  `json:"first_seen_at" gorm:"column:first_seen_at"`
	LastSeenAt   time.Time  `json:"last_seen_at" gorm:"column:last_seen_at"`
	Status       string     `json:"status" gorm:"column:status"`
	LastEventID  *uuid.UUID `json:"last_event_id,omitempty" gorm:"column:last_event_id"`
}

// TableName implementa gorm.Tabler.
func (Group) TableName() string { return TableGroups }

// Event es una muestra de un grupo: el Diagnostic saneado de una ocurrencia.
type Event struct {
	ID            uuid.UUID  `json:"id"`
	GroupID       uuid.UUID  `json:"group_id"`
	CorrelationID string     `json:"correlation_id,omitempty"`
	InvocationID  string     `json:"invocation_id,omitempty"`
	DiagRef       string     `json:"diag_ref,omitempty"`
	IsFirst       bool       `json:"is_first,omitempty"`
	At            time.Time  `json:"at"`
	Diagnostic    Diagnostic `json:"diagnostic"`
}

type eventRow struct {
	ID            uuid.UUID `gorm:"column:id"`
	OrgID         uuid.UUID `gorm:"column:org_id"`
	GroupID       uuid.UUID `gorm:"column:group_id"`
	CorrelationID string    `gorm:"column:correlation_id"`
	InvocationID  string    `gorm:"column:invocation_id"`
	DiagRef       string    `gorm:"column:diag_ref"`
	IsFirst       bool      `gorm:"column:is_first"`
	At            time.Time `gorm:"column:occurred_at"`
	Payload       string    `gorm:"column:payload"`
}

func (eventRow) TableName() string { return TableEvents }

func (r eventRow) event() Event {
	e := Event{ID: r.ID, GroupID: r.GroupID, CorrelationID: r.CorrelationID, InvocationID: r.InvocationID,
		DiagRef: r.DiagRef, IsFirst: r.IsFirst, At: r.At.UTC()}
	// Re-sanea al leer: defensa en profundidad ante filas escritas por otra via.
	var d Diagnostic
	if json.Unmarshal([]byte(r.Payload), &d) == nil {
		d.Org = r.OrgID
		e.Diagnostic = Sanitize(d, nil)
		e.Diagnostic.DiagRef = r.DiagRef
	}
	return e
}

// Store lee y administra los diagnosticos. TODA operacion exige la organizacion
// (la RLS es decorativa en los hosts: el filtro vive aqui) y no existe metodo
// sin org. La escritura de diagnosticos no esta expuesta: solo el Recorder
// escribe, y siempre despues de Sanitize.
type Store struct{ db *gorm.DB }

// NewStore crea un Store sobre db. No toca el esquema (ver Migrate).
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// Filter de List.
type Filter struct {
	Org      uuid.UUID // obligatoria
	Addon    string
	Code     string
	Severity string
	Status   string
	Since    time.Time
	Limit    int
	Offset   int
}

// List devuelve grupos, el mas reciente primero, y el total.
func (s *Store) List(ctx context.Context, f Filter) ([]Group, int64, error) {
	if f.Org == uuid.Nil {
		return nil, 0, ErrOrgRequired
	}
	q := s.db.WithContext(ctx).Model(&Group{}).Where("org_id = ?", f.Org)
	if f.Addon != "" {
		q = q.Where("addon_key = ?", f.Addon)
	}
	if f.Code != "" {
		q = q.Where("code = ?", f.Code)
	}
	if f.Severity != "" {
		q = q.Where("severity = ?", f.Severity)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if !f.Since.IsZero() {
		q = q.Where("last_seen_at >= ?", f.Since.UTC())
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("diag: list: %w", err)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	var out []Group
	if err := q.Order("last_seen_at DESC, id").Limit(f.Limit).Offset(f.Offset).Find(&out).Error; err != nil {
		return nil, 0, fmt.Errorf("diag: list: %w", err)
	}
	return out, total, nil
}

// Get devuelve un grupo y sus muestras (la primera y las ultimas).
func (s *Store) Get(ctx context.Context, org, id uuid.UUID) (Group, []Event, error) {
	if org == uuid.Nil {
		return Group{}, nil, ErrOrgRequired
	}
	var g Group
	err := s.db.WithContext(ctx).Where("org_id = ? AND id = ?", org, id).Take(&g).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Group{}, nil, ErrNotFound
	}
	if err != nil {
		return Group{}, nil, fmt.Errorf("diag: get: %w", err)
	}
	var rows []eventRow
	if err := s.db.WithContext(ctx).Where("org_id = ? AND group_id = ?", org, id).
		Order("occurred_at DESC, id").Limit(MaxSamplesPerGroup + 1).Find(&rows).Error; err != nil {
		return Group{}, nil, fmt.Errorf("diag: get events: %w", err)
	}
	return g, toEvents(rows), nil
}

func toEvents(rows []eventRow) []Event {
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.event())
	}
	return out
}

// EventQuery busca muestras por correlacion o por entidad de negocio.
type EventQuery struct {
	Org           uuid.UUID // obligatoria
	CorrelationID string
	SubjectType   string
	SubjectID     string
	DiagRef       string
	Limit         int
}

// Events devuelve las muestras que casan (al menos un criterio es obligatorio).
func (s *Store) Events(ctx context.Context, f EventQuery) ([]Event, error) {
	if f.Org == uuid.Nil {
		return nil, ErrOrgRequired
	}
	q := s.db.WithContext(ctx).Where("org_id = ?", f.Org)
	n := 0
	if f.CorrelationID != "" {
		q, n = q.Where("correlation_id = ?", f.CorrelationID), n+1
	}
	if f.DiagRef != "" {
		q, n = q.Where("diag_ref = ?", f.DiagRef), n+1
	}
	if f.SubjectType != "" && f.SubjectID != "" {
		q, n = q.Where("subject_type = ? AND subject_id = ?", f.SubjectType, f.SubjectID), n+1
	}
	if n == 0 {
		return nil, errors.New("diag: events needs correlation_id, diag_ref or subject")
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	var rows []eventRow
	if err := q.Order("occurred_at DESC, id").Limit(f.Limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("diag: events: %w", err)
	}
	return toEvents(rows), nil
}

// SetStatus marca un grupo open|muted|resolved. Un grupo resuelto que vuelve a
// ocurrir se reabre solo; uno silenciado sigue silenciado.
func (s *Store) SetStatus(ctx context.Context, org, id uuid.UUID, status string) error {
	if org == uuid.Nil {
		return ErrOrgRequired
	}
	switch status {
	case StatusOpen, StatusMuted, StatusResolved:
	default:
		return ErrBadStatus
	}
	res := s.db.WithContext(ctx).Model(&Group{}).Where("org_id = ? AND id = ?", org, id).Update("status", status)
	if res.Error != nil {
		return fmt.Errorf("diag: set status: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// purge aplica la retencion (mantenimiento global, solo lo invoca el Recorder).
func (s *Store) purge(ctx context.Context, now time.Time, eventAge, groupAge time.Duration) (events, groups int64, err error) {
	ec := now.Add(-eventAge).UTC()
	gc := now.Add(-groupAge).UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// La primera muestra vive tanto como su grupo.
		r := tx.Exec(`DELETE FROM `+TableEvents+` WHERE occurred_at < ? AND is_first = ?`, ec, false)
		if r.Error != nil {
			return r.Error
		}
		events = r.RowsAffected
		r = tx.Exec(`DELETE FROM `+TableEvents+` WHERE group_id IN (SELECT id FROM `+TableGroups+` WHERE last_seen_at < ?)`, gc)
		if r.Error != nil {
			return r.Error
		}
		events += r.RowsAffected
		r = tx.Exec(`DELETE FROM `+TableGroups+` WHERE last_seen_at < ?`, gc)
		if r.Error != nil {
			return r.Error
		}
		groups = r.RowsAffected
		return nil
	})
	return
}
