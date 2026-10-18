package dataload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
}

// MaxRunIssues acota los problemas que se guardan en la corrida; el conteo
// completo queda en Progress.
const MaxRunIssues = 200

// Run es una entrada del historial: una simulación o una aplicación.
type Run struct {
	ID             string              `json:"id"`
	Key            string              `json:"key"` // llave de idempotencia
	Org            string              `json:"org"`
	Mode           RunMode             `json:"mode"`
	State          RunState            `json:"state"`
	Actor          string              `json:"actor,omitempty"`
	Profile        string              `json:"profile"`
	ProfileVersion string              `json:"profileVersion"`
	ProfileHash    string              `json:"profileHash"`
	OverridesHash  string              `json:"overridesHash,omitempty"`
	Source         string              `json:"source"`
	InputHash      string              `json:"inputHash"`
	PlanHash       string              `json:"planHash,omitempty"`
	Progress       Progress            `json:"progress"`
	Issues         []importer.RowIssue `json:"issues,omitempty"`
	Error          string              `json:"error,omitempty"`
	// ReplayOf es el ID de la corrida que ya había resuelto esta misma entrada:
	// esta corrida no escribió nada.
	ReplayOf   string    `json:"replayOf,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// ErrRunNotFound se devuelve cuando la corrida no existe en esa organización.
var ErrRunNotFound = errors.New("dataload: corrida no encontrada")

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

// RunKey es la llave de idempotencia: misma organización, mismo perfil
// resuelto (con sus overrides) y mismo contenido de entrada = misma llave.
func RunKey(org, profileHash, inputHash string, mode RunMode) string {
	sum := sha256.Sum256([]byte(org + "\x00" + profileHash + "\x00" + inputHash + "\x00" + string(mode)))
	return hex.EncodeToString(sum[:])
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
