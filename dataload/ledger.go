package dataload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/asteby/metacore-kernel/importer"
)

// RunState es el estado de una corrida.
type RunState string

const (
	RunQueued    RunState = "queued"
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCanceled  RunState = "canceled"
)

// Terminal indica que la corrida ya no cambiará.
func (s RunState) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCanceled
}

// RunMode distingue la simulación de la aplicación real.
type RunMode string

const (
	ModeDryRun RunMode = "dry_run"
	ModeApply  RunMode = "apply"
	// ModeReview es la aplicación de filas aprobadas tras una revisión.
	ModeReview RunMode = "apply_reviewed"
)

// Progress es el avance observable de una corrida en curso.
type Progress struct {
	Total       int `json:"total"` // filas aplicables (create + update)
	Done        int `json:"done"`  // filas ya escritas
	Created     int `json:"created"`
	Updated     int `json:"updated"`
	Unchanged   int `json:"unchanged"`
	Invalid     int `json:"invalid"`
	Duplicates  int `json:"duplicates"`
	MediaQueued int `json:"mediaQueued"`
	// Review son las filas válidas que esperan una decisión (ActionReview); no
	// se escriben solas.
	Review int `json:"review,omitempty"`
}

// RunLease es el arrendamiento con que un proceso declara que está ejecutando
// una corrida. Si deja de latir y vence, otro proceso puede recuperarla.
type RunLease struct {
	Owner     string    `json:"owner"`
	Heartbeat time.Time `json:"heartbeat"`
	Until     time.Time `json:"until"`
}

// Checkpoint es el punto de reanudación: se guarda después de cada lote escrito.
type Checkpoint struct {
	// Batches y Rows son los lotes y filas ya escritos (acumulados entre reanudaciones).
	Batches int `json:"batches"`
	Rows    int `json:"rows"`
	// LastKey es la identidad de la última fila escrita.
	LastKey string `json:"lastKey,omitempty"`
	// Resumes cuenta cuántas veces se recuperó la corrida tras quedar huérfana.
	Resumes   int       `json:"resumes,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MaxRunIssues acota los problemas que se guardan en la corrida; el conteo
// completo queda en Progress.
const MaxRunIssues = 200

// Run es una entrada del historial: una simulación o una aplicación.
type Run struct {
	ID             string   `json:"id"`
	Key            string   `json:"key"` // llave de idempotencia
	Org            string   `json:"org"`
	Mode           RunMode  `json:"mode"`
	State          RunState `json:"state"`
	Actor          string   `json:"actor,omitempty"`
	Profile        string   `json:"profile"`
	ProfileVersion string   `json:"profileVersion"`
	ProfileHash    string   `json:"profileHash"`
	ExtractHash    string   `json:"extractHash,omitempty"`
	// Registry es la versión del registro de perfiles de la que salió el perfil (si vino de ahí).
	Registry      *RegistryRef        `json:"registry,omitempty"`
	OverridesHash string              `json:"overridesHash,omitempty"`
	Source        string              `json:"source"`
	InputHash     string              `json:"inputHash"`
	PlanHash      string              `json:"planHash,omitempty"`
	Progress      Progress            `json:"progress"`
	Issues        []importer.RowIssue `json:"issues,omitempty"`
	// Review lista (hasta MaxRunIssues) las filas en espera de decisión con su
	// motivo; el conteo completo está en Progress.Review.
	Review []ReviewNote `json:"review,omitempty"`
	// Error es el texto del fallo (idioma por omisión); ErrorCode y ErrorParams lo
	// identifican de forma estable para traducirlo o filtrarlo.
	Error       string `json:"error,omitempty"`
	ErrorCode   Code   `json:"errorCode,omitempty"`
	ErrorParams Params `json:"errorParams,omitempty"`
	// Lease dice qué proceso tiene la corrida (solo con un LeaseLedger). Lo
	// administra el ledger; el ejecutor no lo escribe con Save.
	Lease *RunLease `json:"lease,omitempty"`
	// Checkpoint es el avance durable por lote: lo que una recuperación necesita saber.
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	// ReplayOf es el ID de la corrida que ya había resuelto esta misma entrada:
	// esta corrida no escribió nada.
	ReplayOf   string    `json:"replayOf,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// ErrRunNotFound se devuelve cuando la corrida no existe en esa organización.
var ErrRunNotFound = newErr(CodeRunNotFound)

// Ledger es el historial persistente de corridas; lo implementa ops/addons.
type Ledger interface {
	// Save inserta o actualiza la corrida por ID.
	Save(ctx context.Context, run Run) error
	Get(ctx context.Context, org, id string) (Run, error)
	// FindApplied devuelve la corrida de aplicación más reciente con esa llave
	// que terminó en succeeded (y no es un replay).
	FindApplied(ctx context.Context, org, key string) (Run, bool, error)
	// List devuelve las corridas más recientes primero.
	List(ctx context.Context, org string, limit int) ([]Run, error)
}

// ErrLeaseLost indica que otro proceso recuperó la corrida: quien la ejecutaba
// debe soltarla sin escribir más en el historial.
var ErrLeaseLost = newErr(CodeLeaseLost)

// LeaseLedger es un Ledger que además sabe de arrendamientos. Es opcional: sin
// él, el Runner funciona como antes y no hay recuperación (Recover devuelve
// ErrNoLeaseLedger). Contrato:
//
//   - Save inserta tal cual (con el Lease de la corrida) y al actualizar NO toca
//     el Lease almacenado; una corrida que pasa a estado terminal pierde el Lease.
//   - SaveLeased guarda la corrida de forma atómica solo si el Lease almacenado
//     sigue siendo de owner (y la corrida no es terminal); además lo extiende.
//     Si no, devuelve ErrLeaseLost y no escribe. Es el cerco que impide que un
//     proceso lento pise el resultado del que lo recuperó.
//   - ClaimStale toma de forma atómica corridas queued/running sin arrendamiento o
//     con el arrendamiento vencido, las deja a nombre de owner y suma una
//     reanudación a su Checkpoint. Dos procesos nunca reciben la misma corrida.
type LeaseLedger interface {
	Ledger
	SaveLeased(ctx context.Context, run Run, owner string, now time.Time, ttl time.Duration) error
	// Heartbeat extiende el arrendamiento; false = ya no es de owner.
	Heartbeat(ctx context.Context, org, id, owner string, now time.Time, ttl time.Duration) (bool, error)
	ClaimStale(ctx context.Context, org string, now time.Time, owner string, ttl time.Duration, limit int) ([]Run, error)
	// FindActive devuelve la corrida de aplicación con esa llave que está en curso
	// con arrendamiento vigente.
	FindActive(ctx context.Context, org, key string, now time.Time) (Run, bool, error)
}

// RunKey es la llave de idempotencia: misma organización, mismo perfil
// resuelto (con sus overrides) y mismo contenido de entrada = misma llave.
func RunKey(org, profileHash, inputHash string, mode RunMode) string {
	sum := sha256.Sum256([]byte(org + "\x00" + profileHash + "\x00" + inputHash + "\x00" + string(mode)))
	return hex.EncodeToString(sum[:])
}

// fingerprint es la huella que entra en la llave de idempotencia: la del perfil
// resuelto y, si hay etapa extract, la de su documento.
func fingerprint(profileHash string, ex *Extractor) string {
	if ex == nil {
		return profileHash
	}
	sum := sha256.Sum256([]byte(profileHash + "\x00" + ex.Hash()))
	return hex.EncodeToString(sum[:])
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
