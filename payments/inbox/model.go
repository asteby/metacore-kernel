package inbox

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// Estados of a notification.
const (
	EstadoRecibida         = "recibida"
	EstadoProcesando       = "procesando"
	EstadoProcesada        = "procesada"
	EstadoDuplicada        = "duplicada"
	EstadoIgnorada         = "ignorada"
	EstadoReconciliadaAPI  = "reconciliada_por_api"
	EstadoRequiereRevision = "requiere_revision"
	EstadoReintentar       = "reintentar"
	EstadoDeadLetter       = "dead_letter"
	EstadoResueltaManual   = "resuelta_manual"
)

// Auth verdicts.
const (
	AuthPendiente     = "pendiente"
	AuthFirmaOK       = "firma_ok"
	AuthFirmaInvalida = "firma_invalida"
	AuthSinFirma      = "sin_firma"
	AuthSinSecreto    = "sin_secreto"
	AuthNoVerificable = "no_verificable"
)

// AuthReasonMatchedPrevious marks a signature that only matched the previous
// (rotated-out) secret.
const AuthReasonMatchedPrevious = "matched_previous"

// OpenEstados are the states that still need attention or work.
var OpenEstados = []string{EstadoRecibida, EstadoProcesando, EstadoReintentar, EstadoRequiereRevision, EstadoDeadLetter}

// ReviewEstados are the states a person must look at.
var ReviewEstados = []string{EstadoRequiereRevision, EstadoDeadLetter}

// MaxBodyBytes caps the stored raw body (body_sha256 covers the whole body).
const MaxBodyBytes = 64 << 10

// JSONText is a JSON document stored in a JSONB (Postgres) or TEXT (SQLite)
// column. It marshals as raw JSON.
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
		return fmt.Errorf("inbox: cannot scan %T into JSONText", v)
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
	if j == "" {
		return []byte("{}"), nil
	}
	if !json.Valid([]byte(j)) {
		return json.Marshal(string(j))
	}
	return []byte(j), nil
}

// Notification is one row of commerce_payment_notifications (without
// body_raw, which only Raw returns).
type Notification struct {
	ID               uuid.UUID  `json:"id" gorm:"column:id"`
	OrganizationID   uuid.UUID  `json:"organization_id" gorm:"column:organization_id"`
	Provider         string     `json:"provider" gorm:"column:provider"`
	Route            string     `json:"route" gorm:"column:route"`
	ReceivedAt       time.Time  `json:"received_at" gorm:"column:received_at"`
	SourceIP         string     `json:"source_ip,omitempty" gorm:"column:source_ip"`
	Method           string     `json:"method" gorm:"column:method"`
	Query            JSONText   `json:"query" gorm:"column:query"`
	Headers          JSONText   `json:"headers" gorm:"column:headers"`
	BodySHA256       string     `json:"body_sha256" gorm:"column:body_sha256"`
	BodyTruncated    bool       `json:"body_truncated" gorm:"column:body_truncated"`
	BodyRedacted     JSONText   `json:"body_redacted" gorm:"column:body_redacted"`
	ResourceKind     string     `json:"resource_kind,omitempty" gorm:"column:resource_kind"`
	ResourceID       string     `json:"resource_id,omitempty" gorm:"column:resource_id"`
	EventID          string     `json:"event_id,omitempty" gorm:"column:event_id"`
	Auth             string     `json:"auth" gorm:"column:auth"`
	AuthReason       string     `json:"auth_reason,omitempty" gorm:"column:auth_reason"`
	Estado           string     `json:"estado" gorm:"column:estado"`
	Motivo           string     `json:"motivo,omitempty" gorm:"column:motivo"`
	Intentos         int        `json:"intentos" gorm:"column:intentos"`
	ProximoIntentoAt *time.Time `json:"proximo_intento_at,omitempty" gorm:"column:proximo_intento_at"`
	ProcesadaAt      *time.Time `json:"procesada_at,omitempty" gorm:"column:procesada_at"`
	PaymentID        *uuid.UUID `json:"payment_id,omitempty" gorm:"column:payment_id"`
	CheckoutRef      string     `json:"checkout_ref,omitempty" gorm:"column:checkout_ref"`
	IncidentID       *uuid.UUID `json:"incident_id,omitempty" gorm:"column:incident_id"`
	ReplayOf         *uuid.UUID `json:"replay_of,omitempty" gorm:"column:replay_of"`
	DedupeKey        string     `json:"dedupe_key,omitempty" gorm:"column:dedupe_key"`
	PurgedAt         *time.Time `json:"purged_at,omitempty" gorm:"column:purged_at"`
	UpdatedAt        time.Time  `json:"updated_at" gorm:"column:updated_at"`
}

// TableName pins the GORM table.
func (Notification) TableName() string { return TableNotifications }

// notificationColumns lists every column except body_raw.
const notificationColumns = `id, organization_id, provider, route, received_at, source_ip, method, query, headers,
	body_sha256, body_truncated, body_redacted, resource_kind, resource_id, event_id, auth, auth_reason, estado,
	motivo, intentos, proximo_intento_at, procesada_at, payment_id, checkout_ref, incident_id, replay_of,
	dedupe_key, purged_at, updated_at`

// Attempt is one processing attempt of a notification.
type Attempt struct {
	NotificationID uuid.UUID  `json:"notification_id" gorm:"column:notification_id"`
	N              int        `json:"n" gorm:"column:n"`
	OrganizationID uuid.UUID  `json:"organization_id" gorm:"column:organization_id"`
	StartedAt      time.Time  `json:"started_at" gorm:"column:started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty" gorm:"column:ended_at"`
	Resultado      string     `json:"resultado" gorm:"column:resultado"`
	Motivo         string     `json:"motivo,omitempty" gorm:"column:motivo"`
	Actor          string     `json:"actor" gorm:"column:actor"`
	HTTPStatus     int        `json:"http_status,omitempty" gorm:"column:http_status"`
}

// TableName pins the GORM table.
func (Attempt) TableName() string { return TableAttempts }

// Request is an inbound webhook request as the host saw it.
type Request struct {
	Org      uuid.UUID
	Provider string
	Route    string
	Method   string
	Query    url.Values
	Headers  http.Header
	Body     []byte
	// SourceIP is the real client IP (see SourceIP).
	SourceIP   string
	ReceivedAt time.Time
}

// WriteFailure is a write the guest could not complete on its own tables
// ("could not update payment"), reported in its structured result so the
// kernel can open an incident instead of letting it die in a log.
type WriteFailure struct {
	Table string `json:"table"`
	Row   string `json:"row,omitempty"`
	Cause string `json:"cause"`
}

// Result is the outcome Resolve stores.
type Result struct {
	Auth         string
	AuthReason   string
	ResourceKind string
	ResourceID   string
	EventID      string
	Estado       string
	Motivo       string
	PaymentID    *uuid.UUID
	CheckoutRef  string
	HTTPStatus   int
	// Actor is "system" (default) or a user id for a manual run.
	Actor string
	// RetryAfter overrides the backoff step for Estado=reintentar.
	RetryAfter    time.Duration
	WriteFailures []WriteFailure
	// Amount and Currency enrich incidents (optional).
	Amount   string
	Currency string
}
