package dataload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Filas "revisar": la etapa extract puede encontrar que una fila es válida pero
// dudosa (no se pudo leer con certeza). Esas filas NO entran a la carga
// automática: el plan las marca ActionReview con su motivo, la corrida las
// reporta (Progress.Review y Run.Review) y, si hay ReviewStore, quedan guardadas
// con el registro exacto que se habría escrito. Una persona decide después
// (Decide) y lo aprobado se escribe con ApplyApproved. La decisión queda atada a
// la huella de la fila: si el archivo cambia la fila, la decisión vieja no vale.

// ReviewNote es una fila en espera tal como se lista en la corrida.
type ReviewNote struct {
	Row     int          `json:"row"`
	Key     string       `json:"key"`
	Outcome Action       `json:"outcome,omitempty"`
	Items   []ReviewItem `json:"items"`
}

// ReviewState es el estado de una fila en revisión.
type ReviewState string

const (
	ReviewPending    ReviewState = "pending"
	ReviewApproved   ReviewState = "approved" // decidida, todavía sin escribir
	ReviewRejected   ReviewState = "rejected"
	ReviewApplied    ReviewState = "applied"
	ReviewSuperseded ReviewState = "superseded" // una corrida posterior ya la resolvió sin intervención
)

// ReviewEntry es una fila guardada para decidir después.
type ReviewEntry struct {
	Org    string `json:"org"`
	Entity string `json:"entity"`
	Key    string `json:"key"`
	// Perfil con el que se planificó (para el historial de la aplicación posterior).
	Profile        string              `json:"profile"`
	ProfileVersion string              `json:"profileVersion"`
	ProfileHash    string              `json:"profileHash"`
	RunID          string              `json:"runId"`
	Row            int                 `json:"row"`
	Hash           string              `json:"hash"`
	Record         map[string]any      `json:"record"`
	Media          map[string][]string `json:"media,omitempty"`
	Items          []ReviewItem        `json:"items"`
	Outcome        Action              `json:"outcome,omitempty"`
	State          ReviewState         `json:"state"`
	DecidedBy      string              `json:"decidedBy,omitempty"`
	DecidedAt      time.Time           `json:"decidedAt,omitempty"`
	Note           string              `json:"note,omitempty"`
	AppliedRun     string              `json:"appliedRun,omitempty"`
	CreatedAt      time.Time           `json:"createdAt"`
	UpdatedAt      time.Time           `json:"updatedAt"`
}

// ReviewFilter acota List.
type ReviewFilter struct {
	Entity string
	State  ReviewState
	RunID  string
	// Code deja solo las filas que traen ese código de revisión.
	Code  string
	Limit int
}

// ReviewStore guarda las filas en espera; lo implementan ops/addons.
type ReviewStore interface {
	// Put inserta o reemplaza la entrada de (Org, Entity, Key).
	Put(ctx context.Context, e ReviewEntry) error
	Get(ctx context.Context, org, entity, key string) (ReviewEntry, bool, error)
	// List devuelve las entradas de la organización por orden de fila.
	List(ctx context.Context, org string, f ReviewFilter) ([]ReviewEntry, error)
}

// Errores de revisión.
var (
	ErrReviewNotFound = errors.New("dataload: la fila no está en revisión")
	// ErrReviewStale: la decisión se tomó sobre una versión de la fila que ya no es la vigente.
	ErrReviewStale = errors.New("dataload: la fila cambió desde que se revisó; decide sobre la versión vigente")
	// ErrReviewClosed: la fila ya se aplicó o la resolvió otra corrida.
	ErrReviewClosed = errors.New("dataload: la fila ya no está abierta a decisión")
)

func (p *Plan) reviewNotes() []ReviewNote {
	var out []ReviewNote
	for _, r := range p.Rows {
		if r.Action != ActionReview {
			continue
		}
		if len(out) == MaxRunIssues {
			break
		}
		out = append(out, ReviewNote{Row: r.Row, Key: r.Key, Outcome: r.Outcome, Items: r.Review})
	}
	return out
}

// holdReviews guarda las filas ActionReview del plan. Una decisión previa se
// conserva solo si la fila no cambió (misma huella). Las pendientes que esta
// corrida ya resolvió sin intervención pasan a superseded.
func (r *Runner) holdReviews(ctx context.Context, run Run, plan *Plan) error {
	if r.Reviews == nil {
		return nil
	}
	now := r.now()
	inReview := map[string]bool{}
	for _, row := range plan.Rows {
		if row.Action != ActionReview {
			continue
		}
		inReview[row.Key] = true
		e := ReviewEntry{
			Org: plan.Org, Entity: plan.Entity, Key: row.Key, Profile: run.Profile, ProfileVersion: run.ProfileVersion, ProfileHash: run.ProfileHash,
			RunID: run.ID, Row: row.Row, Hash: row.Hash, Record: row.Record, Media: row.Media, Items: row.Review, Outcome: row.Outcome,
			State: ReviewPending, CreatedAt: now, UpdatedAt: now,
		}
		prev, ok, err := r.Reviews.Get(ctx, plan.Org, plan.Entity, row.Key)
		if err != nil {
			return fmt.Errorf("dataload: revisión: %w", err)
		}
		if ok && prev.Hash == row.Hash && prev.State != ReviewApplied && prev.State != ReviewSuperseded {
			e.State, e.DecidedBy, e.DecidedAt, e.Note, e.CreatedAt = prev.State, prev.DecidedBy, prev.DecidedAt, prev.Note, prev.CreatedAt
		}
		if err := r.Reviews.Put(ctx, e); err != nil {
			return fmt.Errorf("dataload: revisión: %w", err)
		}
	}
	pending, err := r.Reviews.List(ctx, plan.Org, ReviewFilter{Entity: plan.Entity, State: ReviewPending})
	if err != nil {
		return fmt.Errorf("dataload: revisión: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range plan.Rows {
		if row.Action != ActionInvalid && row.Action != ActionReview && row.Key != "" {
			seen[row.Key] = true
		}
	}
	for _, e := range pending {
		if seen[e.Key] && !inReview[e.Key] {
			e.State, e.UpdatedAt = ReviewSuperseded, now
			if err := r.Reviews.Put(ctx, e); err != nil {
				return fmt.Errorf("dataload: revisión: %w", err)
			}
		}
	}
	return nil
}

// ReviewDecision es la decisión explícita de una persona sobre una fila.
type ReviewDecision struct {
	Entity string
	Key    string
	// Hash debe ser la huella de la fila que la persona vio (ReviewEntry.Hash).
	Hash    string
	Approve bool
	Note    string
}

// Decide registra decisiones sin escribir nada en el destino. Es todo o nada:
// si una decisión no corresponde a la versión vigente de su fila, no se registra ninguna.
func (r *Runner) Decide(ctx context.Context, org, actor string, ds []ReviewDecision) ([]ReviewEntry, error) {
	if r.Reviews == nil {
		return nil, errors.New("dataload: el runner no tiene ReviewStore")
	}
	if org == "" || actor == "" {
		return nil, errors.New("dataload: una decisión necesita org y quién decide")
	}
	var out []ReviewEntry
	for _, d := range ds {
		e, ok, err := r.Reviews.Get(ctx, org, d.Entity, d.Key)
		if err != nil {
			return nil, err
		}
		switch {
		case !ok:
			return nil, fmt.Errorf("%w: %s/%s", ErrReviewNotFound, d.Entity, d.Key)
		case e.State == ReviewApplied || e.State == ReviewSuperseded:
			return nil, fmt.Errorf("%w: %s (%s)", ErrReviewClosed, d.Key, e.State)
		case e.Hash != d.Hash:
			return nil, fmt.Errorf("%w: %s", ErrReviewStale, d.Key)
		}
		e.State = ReviewRejected
		if d.Approve {
			e.State = ReviewApproved
		}
		e.DecidedBy, e.DecidedAt, e.Note, e.UpdatedAt = actor, r.now(), d.Note, r.now()
		out = append(out, e)
	}
	for _, e := range out {
		if err := r.Reviews.Put(ctx, e); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ApplyApproved escribe en el destino las filas aprobadas de una entidad y deja
// constancia en el historial (ModeReview). Es idempotente: el destino hace upsert
// por identidad y lo ya idéntico no se reescribe. Los medios de la fila se encolan.
func (r *Runner) ApplyApproved(ctx context.Context, org, actor, entity string) (Run, error) {
	if r.Reviews == nil {
		return Run{}, errors.New("dataload: el runner no tiene ReviewStore")
	}
	entries, err := r.Reviews.List(ctx, org, ReviewFilter{Entity: entity, State: ReviewApproved})
	if err != nil {
		return Run{}, err
	}
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s|%s\n", e.Key, e.Hash)
	}
	run := Run{
		ID: newID(), Org: org, Mode: ModeReview, State: RunRunning, Actor: actor, Source: "revisión", CreatedAt: r.now(), StartedAt: r.now(),
		Key: RunKey(org, "review", hex.EncodeToString(h.Sum(nil)), ModeReview), InputHash: hex.EncodeToString(h.Sum(nil)),
	}
	if len(entries) > 0 {
		run.Profile, run.ProfileVersion, run.ProfileHash = entries[0].Profile, entries[0].ProfileVersion, entries[0].ProfileHash
	}
	fail := func(err error) (Run, error) {
		run.State, run.Error, run.FinishedAt = RunFailed, err.Error(), r.now()
		_ = r.Ledger.Save(context.WithoutCancel(ctx), run)
		return run, err
	}
	run.Progress.Total = len(entries)
	for lo := 0; lo < len(entries); lo += r.batch() {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		batch := entries[lo:min(lo+r.batch(), len(entries))]
		keys := make([]string, len(batch))
		for i, e := range batch {
			keys[i] = e.Key
		}
		existing, err := r.Target.Lookup(ctx, org, entity, keys)
		if err != nil {
			return fail(fmt.Errorf("dataload: consulta al destino: %w", err))
		}
		var changes []Change
		for _, e := range batch {
			ex, found := existing[e.Key]
			switch {
			case !found:
				changes = append(changes, Change{Key: e.Key, Action: ActionCreate, Record: e.Record, Hash: e.Hash})
				run.Progress.Created++
			case ex.Hash == e.Hash:
				run.Progress.Unchanged++
			default:
				changes = append(changes, Change{Key: e.Key, Action: ActionUpdate, Record: e.Record, Hash: e.Hash})
				run.Progress.Updated++
			}
		}
		if len(changes) > 0 {
			if err := r.Target.Apply(ctx, org, entity, changes); err != nil {
				return fail(fmt.Errorf("dataload: aplicar filas aprobadas: %w", err))
			}
		}
		for _, e := range batch {
			e.State, e.AppliedRun, e.UpdatedAt = ReviewApplied, run.ID, r.now()
			if err := r.Reviews.Put(ctx, e); err != nil {
				return fail(err)
			}
			run.Progress.Done++
			if r.Media != nil && len(e.Media) > 0 {
				n, err := r.Media.Enqueue(ctx, r.Media.jobsFor(org, entity, run.ID, e.Key, e.Media))
				if err != nil {
					return fail(fmt.Errorf("dataload: encolar medios: %w", err))
				}
				run.Progress.MediaQueued += n
			}
		}
		_ = r.Ledger.Save(ctx, run)
	}
	run.State, run.FinishedAt = RunSucceeded, r.now()
	return run, r.Ledger.Save(ctx, run)
}

// MemoryReviewStore es un ReviewStore en memoria (pruebas y desarrollo).
type MemoryReviewStore struct {
	mu sync.Mutex
	m  map[string]ReviewEntry
}

func NewMemoryReviewStore() *MemoryReviewStore {
	return &MemoryReviewStore{m: map[string]ReviewEntry{}}
}

func (s *MemoryReviewStore) Put(_ context.Context, e ReviewEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[mk(e.Org, e.Entity, e.Key)] = e
	return nil
}

func (s *MemoryReviewStore) Get(_ context.Context, org, entity, key string) (ReviewEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[mk(org, entity, key)]
	return e, ok, nil
}

func (s *MemoryReviewStore) List(_ context.Context, org string, f ReviewFilter) ([]ReviewEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ReviewEntry
	for _, e := range s.m {
		if e.Org != org || (f.Entity != "" && e.Entity != f.Entity) || (f.State != "" && e.State != f.State) || (f.RunID != "" && e.RunID != f.RunID) {
			continue
		}
		if f.Code != "" {
			has := false
			for _, it := range e.Items {
				if it.Code == f.Code {
					has = true
				}
			}
			if !has {
				continue
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return strings.Compare(out[i].Key, out[j].Key) < 0
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}
