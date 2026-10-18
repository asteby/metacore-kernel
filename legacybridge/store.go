package legacybridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RefState es el estado del mapeo de una fila.
type RefState string

const (
	// RefSynced: los dos lados coinciden en la versión guardada.
	RefSynced RefState = "synced"
	// RefPendingLegacy: la fila nació o cambió en el host y el comando hacia el
	// legado (su dueño) todavía no se confirma. Se puede usar localmente.
	RefPendingLegacy RefState = "pending_legacy"
	// RefDeleted: el origen la borró; en el host quedó con borrado lógico.
	RefDeleted RefState = "deleted"
)

// Ref es una fila de bridge_refs: el mapa de ids entre el host y un sistema.
// Únicos: (org, system, entity, external_id) y (org, system, entity, local_id).
type Ref struct {
	Org        string `json:"org"`
	System     string `json:"system"`
	Entity     string `json:"entity"`
	ExternalID string `json:"external_id"`
	LocalID    string `json:"local_id"`
	// Owner es el dueño con el que se escribió la última vez (informativo: el
	// dueño vigente lo dice el mapa de propiedad).
	Owner         Owner     `json:"owner"`
	SourceVersion string    `json:"source_version"`
	LastHash      string    `json:"last_hash"`
	CreatedIn     Owner     `json:"created_in"`
	State         RefState  `json:"state"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// withDefaults completa dueño, origen y estado vacíos.
func (r Ref) withDefaults() Ref {
	if r.Owner == "" {
		r.Owner = OwnerLegacy
	}
	if r.CreatedIn == "" {
		r.CreatedIn = OwnerLegacy
	}
	if r.State == "" {
		r.State = RefSynced
	}
	return r
}

func (r Ref) validate() error {
	if r.Org == "" || r.System == "" || r.Entity == "" || r.ExternalID == "" || r.LocalID == "" {
		return fmt.Errorf("legacybridge: ref incompleta (org, system, entity, external_id y local_id son obligatorios): %s|%s|%s", r.Entity, r.ExternalID, r.LocalID)
	}
	return nil
}

// RefStore guarda bridge_refs.
type RefStore interface {
	GetRef(ctx context.Context, org, system, entity, externalID string) (Ref, bool, error)
	GetRefByLocal(ctx context.Context, org, system, entity, localID string) (Ref, bool, error)
	// PutRef inserta o reemplaza por (org, system, entity, external_id).
	// ErrRefConflict si el local_id ya está mapeado a otro external_id.
	PutRef(ctx context.Context, r Ref) error
}

// ErrRefConflict: el local_id ya está mapeado a otro external_id (duplicado
// lógico; se resuelve con una fusión manual).
var ErrRefConflict = errors.New("legacybridge: local_id ya mapeado a otro external_id")

// CursorStore guarda la marca de agua de cada flujo de un origen.
type CursorStore interface {
	GetCursor(ctx context.Context, org, system, stream string) (string, error)
	SetCursor(ctx context.Context, org, system, stream, cursor string) error
}

// OwnershipStore guarda el mapa de propiedad y la fase activa.
type OwnershipStore interface {
	Rules(ctx context.Context, org, system string) ([]OwnershipRule, error)
	// PutRule inserta o reemplaza por (org, system, entity, branch, phase) y deja
	// la versión anterior en la bitácora.
	PutRule(ctx context.Context, r OwnershipRule) error
	Phase(ctx context.Context, org, system string) (string, error)
	SetPhase(ctx context.Context, org, system, phase, actor string) error
}

// PendingEnvelope es un envelope en espera de sus dependencias.
type PendingEnvelope struct {
	Org         string       `json:"org"`
	System      string       `json:"system"`
	Envelope    Envelope     `json:"envelope"`
	Missing     []Dependency `json:"missing"`
	Attempts    int          `json:"attempts"`
	FirstSeen   time.Time    `json:"first_seen"`
	NextAttempt time.Time    `json:"next_attempt"`
}

// PendingStore guarda los envelopes en espera. Uno por (org, system, entity,
// external_id): uno nuevo reemplaza al anterior.
type PendingStore interface {
	PutPending(ctx context.Context, p PendingEnvelope) error
	GetPending(ctx context.Context, org, system, entity, externalID string) (PendingEnvelope, bool, error)
	DeletePending(ctx context.Context, org, system, entity, externalID string) error
	// DuePending lista hasta limit envelopes con NextAttempt <= now, los más viejos primero.
	DuePending(ctx context.Context, now time.Time, limit int) ([]PendingEnvelope, error)
	CountPending(ctx context.Context, org, system string) (int64, error)
}

// CommandStatus es el estado de un comando hacia el legado.
type CommandStatus string

const (
	CommandPending   CommandStatus = "pending"
	CommandSent      CommandStatus = "sent"
	CommandDead      CommandStatus = "dead"
	CommandDiscarded CommandStatus = "discarded"
)

// Command es una fila de bridge_commands.
type Command struct {
	ID     string `json:"id"`
	Org    string `json:"org"`
	System string `json:"system"`
	// Kind es el comando del legado (p. ej. "customers.upsert"); Connector lo
	// traduce a su ruta.
	Kind string `json:"kind"`
	// AggregateKey ordena: los comandos de un mismo agregado salen en orden de Seq.
	AggregateKey string `json:"aggregate_key"`
	Seq          int64  `json:"seq"`
	// IdempotencyKey es única por (org, system); viaja como X-Idempotency-Key.
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload"`
	// Entity y LocalID (opcionales) identifican la fila del host: al confirmarse,
	// el external_id y la versión que devuelve el legado se registran en
	// bridge_refs (supresión del eco).
	Entity        string          `json:"entity,omitempty"`
	LocalID       string          `json:"local_id,omitempty"`
	Status        CommandStatus   `json:"status"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error,omitempty"`
	Response      json.RawMessage `json:"response,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// CommandFilter acota ListCommands.
type CommandFilter struct {
	Org    string
	System string
	Status CommandStatus
	Limit  int
}

// AggregateRef identifica la cola de un agregado.
type AggregateRef struct {
	Org          string `json:"org"`
	System       string `json:"system"`
	AggregateKey string `json:"aggregate_key"`
}

// CommandStore guarda bridge_commands.
type CommandStore interface {
	// InsertCommand asigna ID, Seq (siguiente del agregado) y fechas. Si la
	// IdempotencyKey ya existe devuelve el comando guardado y created=false.
	InsertCommand(ctx context.Context, c Command) (Command, bool, error)
	GetCommand(ctx context.Context, id string) (Command, bool, error)
	// HeadCommand es el comando no entregado de menor Seq del agregado (pending
	// o dead: un dead bloquea su agregado). false si no queda ninguno.
	HeadCommand(ctx context.Context, a AggregateRef) (Command, bool, error)
	UpdateCommand(ctx context.Context, c Command) error
	ListCommands(ctx context.Context, f CommandFilter) ([]Command, error)
	// DueAggregates lista agregados cuyo comando de cabeza está pending y vence
	// antes de now.
	DueAggregates(ctx context.Context, now time.Time, limit int) ([]AggregateRef, error)
	CountCommands(ctx context.Context, org, system string, status CommandStatus) (int64, error)
}

// Store junta todos los almacenes (GormStore y MemoryStore los implementan).
type Store interface {
	RefStore
	CursorStore
	OwnershipStore
	PendingStore
	CommandStore
}
