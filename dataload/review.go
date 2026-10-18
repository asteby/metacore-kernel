package dataload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// ReviewSourceName es el nombre de fuente de las corridas ModeReview en el historial.
const ReviewSourceName = "review"

// Errores de revisión.
var (
	ErrReviewNotFound = newErr(CodeReviewNotFound)
	// ErrReviewStale: la decisión se tomó sobre una versión de la fila que ya no es la vigente.
	ErrReviewStale = newErr(CodeReviewStale)
	// ErrReviewClosed: la fila ya se aplicó o la resolvió otra corrida.
	ErrReviewClosed = newErr(CodeReviewClosed)
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
	if err := r.holdReviewRows(ctx, run, plan.Org, plan.Entity, plan.Rows); err != nil {
		return err
	}
	inReview, seen := map[string]bool{}, map[string]bool{}
	for _, row := range plan.Rows {
		switch {
		case row.Action == ActionReview:
			inReview[row.Key] = true
		case row.Action != ActionInvalid && row.Key != "":
			seen[row.Key] = true
		}
	}
	return r.supersedeReviews(ctx, plan.Org, plan.Entity, func(key string) bool { return seen[key] && !inReview[key] })
}

// holdReviewRows guarda las filas ActionReview de rows (una página o el plan entero).
func (r *Runner) holdReviewRows(ctx context.Context, run Run, org, entity string, rows []RowPlan) error {
	if r.Reviews == nil {
		return nil
	}
	now := r.now()
	for _, row := range rows {
		if row.Action != ActionReview {
			continue
		}
		e := ReviewEntry{
			Org: org, Entity: entity, Key: row.Key, Profile: run.Profile, ProfileVersion: run.ProfileVersion, ProfileHash: run.ProfileHash,
			RunID: run.ID, Row: row.Row, Hash: row.Hash, Record: row.Record, Media: row.Media, Items: row.Review, Outcome: row.Outcome,
			State: ReviewPending, CreatedAt: now, UpdatedAt: now,
		}
		prev, ok, err := r.Reviews.Get(ctx, org, entity, row.Key)
		if err != nil {
			return wrapErr(err, CodeReviewStore)
		}
		if ok && prev.Hash == row.Hash && prev.State != ReviewApplied && prev.State != ReviewSuperseded {
			e.State, e.DecidedBy, e.DecidedAt, e.Note, e.CreatedAt = prev.State, prev.DecidedBy, prev.DecidedAt, prev.Note, prev.CreatedAt
		}
		if err := r.Reviews.Put(ctx, e); err != nil {
			return wrapErr(err, CodeReviewStore)
		}
	}
	return nil
}

// supersedeReviews pasa a superseded las pendientes que esta corrida resolvió
// sin intervención (resolved(key): la trajo y ya no pide revisión).
func (r *Runner) supersedeReviews(ctx context.Context, org, entity string, resolved func(string) bool) error {
	if r.Reviews == nil {
		return nil
	}
	now := r.now()
	pending, err := r.Reviews.List(ctx, org, ReviewFilter{Entity: entity, State: ReviewPending})
	if err != nil {
		return wrapErr(err, CodeReviewStore)
	}
	for _, e := range pending {
		if resolved(e.Key) {
			e.State, e.UpdatedAt = ReviewSuperseded, now
			if err := r.Reviews.Put(ctx, e); err != nil {
				return wrapErr(err, CodeReviewStore)
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
		return nil, newErr(CodeNoReviewStore)
	}
	// Se pregunta por cada entidad con sus identidades: mezclar entidades en una
	// llamada no esquiva el permiso de ninguna.
	byEntity := map[string][]string{}
	for _, d := range ds {
		if d.Entity == "" {
			return nil, newErr(CodeEntityRequired, "action", msg(ActionReviewDecide))
		}
		byEntity[d.Entity] = append(byEntity[d.Entity], d.Key)
	}
	entities := make([]string, 0, len(byEntity))
	for e := range byEntity {
		entities = append(entities, e)
	}
	sort.Strings(entities)
	if len(entities) == 0 {
		entities = []string{""} // sin decisiones: igual se exige org, actor y permiso
	}
	for _, e := range entities {
		if err := r.authorize(ctx, AuthRequest{Org: org, Actor: actor, Action: ActionReviewDecide, Entity: e, Keys: byEntity[e]}); err != nil {
			return nil, err
		}
	}
	var out []ReviewEntry
	for _, d := range ds {
		e, ok, err := r.Reviews.Get(ctx, org, d.Entity, d.Key)
		if err != nil {
			return nil, err
		}
		switch {
		case !ok:
			return nil, newErr(CodeReviewNotFound, "entity", d.Entity, "key", d.Key)
		case e.State == ReviewApplied || e.State == ReviewSuperseded:
			return nil, newErr(CodeReviewClosed, "key", d.Key, "state", e.State)
		case e.Hash != d.Hash:
			return nil, newErr(CodeReviewStale, "key", d.Key)
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
		return Run{}, newErr(CodeNoReviewStore)
	}
	if entity == "" {
		// Con "" el almacén listaría las aprobadas de todas las entidades y se
		// escribirían bajo un nombre vacío.
		return Run{}, newErr(CodeEntityRequired, "action", msg(ActionReviewApply))
	}
	entries, err := r.Reviews.List(ctx, org, ReviewFilter{Entity: entity, State: ReviewApproved})
	if err != nil {
		return Run{}, err
	}
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	if err := r.authorize(ctx, AuthRequest{Org: org, Actor: actor, Action: ActionReviewApply, Entity: entity, Keys: keys}); err != nil {
		return Run{}, err
	}
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s|%s\n", e.Key, e.Hash)
	}
	run := Run{
		ID: newID(), Org: org, Mode: ModeReview, State: RunRunning, Actor: actor, Source: ReviewSourceName, CreatedAt: r.now(), StartedAt: r.now(),
		Key: RunKey(org, "review", hex.EncodeToString(h.Sum(nil)), ModeReview), InputHash: hex.EncodeToString(h.Sum(nil)),
	}
	if len(entries) > 0 {
		run.Profile, run.ProfileVersion, run.ProfileHash = entries[0].Profile, entries[0].ProfileVersion, entries[0].ProfileHash
	}
	fail := func(err error) (Run, error) {
		run.State, run.FinishedAt = RunFailed, r.now()
		run.setError(err)
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
			return fail(wrapErr(err, CodeTargetLookup))
		}
		// Lo listado puede haber cambiado mientras tanto (alguien la rechazó, o
		// una carga nueva trajo otra versión): solo se aplica lo que sigue
		// aprobado con la misma huella.
		var live []ReviewEntry
		for _, e := range batch {
			if r.stillApproved(ctx, e) {
				live = append(live, e)
			} else {
				run.Progress.Total--
			}
		}
		batch = live
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
				return fail(wrapErr(err, CodeApplyApproved))
			}
		}
		for _, e := range batch {
			if !r.stillApproved(ctx, e) {
				continue // cambió durante la escritura: su nuevo estado no se pisa
			}
			e.State, e.AppliedRun, e.UpdatedAt = ReviewApplied, run.ID, r.now()
			if err := r.Reviews.Put(ctx, e); err != nil {
				return fail(err)
			}
			run.Progress.Done++
			if r.Media != nil && len(e.Media) > 0 {
				n, err := r.Media.Enqueue(ctx, r.Media.jobsFor(org, entity, run.ID, e.Key, e.Media))
				if err != nil {
					return fail(wrapErr(err, CodeMediaEnqueue))
				}
				run.Progress.MediaQueued += n
			}
		}
		_ = r.Ledger.Save(ctx, run)
	}
	run.State, run.FinishedAt = RunSucceeded, r.now()
	return run, r.Ledger.Save(ctx, run)
}

// stillApproved dice si la entrada sigue aprobada y con la misma huella en el
// almacén. El ReviewStore no ofrece compare-and-swap, así que esto acota la
// ventana de carrera con Decide y con otra carga, no la cierra del todo.
func (r *Runner) stillApproved(ctx context.Context, e ReviewEntry) bool {
	cur, ok, err := r.Reviews.Get(ctx, e.Org, e.Entity, e.Key)
	return err == nil && ok && cur.State == ReviewApproved && cur.Hash == e.Hash
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
