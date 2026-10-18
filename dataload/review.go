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
	ReviewPending  ReviewState = "pending"
	ReviewApproved ReviewState = "approved" // decidida, todavía sin escribir
	// ReviewApplying: reservada por un ApplyApproved que la está escribiendo
	// (solo con ConditionalReviewStore). Nadie más la decide mientras tanto; si el
	// proceso muere, RecoverApplying la deja en applied o de vuelta en approved.
	ReviewApplying   ReviewState = "applying"
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
	Profile        string `json:"profile"`
	ProfileVersion string `json:"profileVersion"`
	ProfileHash    string `json:"profileHash"`
	RunID          string `json:"runId"`
	Row            int    `json:"row"`
	Hash           string `json:"hash"`
	// BaseHash es la huella que tenía el destino para esta identidad cuando se
	// planificó la fila (vacía si no existía o el destino no guarda huellas).
	// ApplyApproved no escribe si el destino cambió desde entonces.
	BaseHash   string              `json:"baseHash,omitempty"`
	Record     map[string]any      `json:"record"`
	Media      map[string][]string `json:"media,omitempty"`
	Items      []ReviewItem        `json:"items"`
	Outcome    Action              `json:"outcome,omitempty"`
	State      ReviewState         `json:"state"`
	DecidedBy  string              `json:"decidedBy,omitempty"`
	DecidedAt  time.Time           `json:"decidedAt,omitempty"`
	Note       string              `json:"note,omitempty"`
	AppliedRun string              `json:"appliedRun,omitempty"`
	CreatedAt  time.Time           `json:"createdAt"`
	UpdatedAt  time.Time           `json:"updatedAt"`
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

// ReviewExpect es lo que ConditionalReviewStore.PutIf espera encontrar en el
// almacén para (Org, Entity, Key) de la entrada que se escribe.
type ReviewExpect struct {
	// Missing: la entrada no debe existir (inserción). Si es true, State y Hash no cuentan.
	Missing bool
	// State y Hash deben coincidir con los de la entrada guardada.
	State ReviewState
	Hash  string
}

// ConditionalReviewStore es una extensión OPCIONAL de ReviewStore con escritura
// condicional (compare-and-swap). Con ella el Runner cierra del todo la carrera
// entre Decide y ApplyApproved: ApplyApproved reserva cada fila (approved ->
// applying) antes de escribir al destino y Decide no puede rechazar una fila
// reservada. Sin ella el Runner solo acota la ventana releyendo el estado (ver
// stillApproved) y una decisión que llega en medio de la escritura puede quedar
// como 'rejected' con el dato ya escrito.
//
// PutIf escribe e solo si la entrada guardada coincide con expect, de forma
// ATÓMICA respecto de cualquier otro Put/PutIf (en SQL: UPDATE ... WHERE state =
// $s AND hash = $h, o INSERT ... ON CONFLICT DO NOTHING para Missing).
// Devuelve true si escribió; false (sin error) si no coincidía.
type ConditionalReviewStore interface {
	ReviewStore
	PutIf(ctx context.Context, e ReviewEntry, expect ReviewExpect) (bool, error)
}

// expectOf es la condición "la entrada sigue como la leí".
func expectOf(e ReviewEntry) ReviewExpect { return ReviewExpect{State: e.State, Hash: e.Hash} }

// casReviews devuelve el almacén con escritura condicional, si lo es.
func (r *Runner) casReviews() ConditionalReviewStore {
	c, _ := r.Reviews.(ConditionalReviewStore)
	return c
}

// casRetries acota los reintentos de una escritura condicional que choca con otra.
const casRetries = 8

// ReviewSourceName es el nombre de fuente de las corridas ModeReview en el historial.
const ReviewSourceName = "review"

// Errores de revisión.
var (
	ErrReviewNotFound = newErr(CodeReviewNotFound)
	// ErrReviewStale: la decisión se tomó sobre una versión de la fila que ya no es la vigente.
	ErrReviewStale = newErr(CodeReviewStale)
	// ErrReviewClosed: la fila ya se aplicó o la resolvió otra corrida.
	ErrReviewClosed = newErr(CodeReviewClosed)
	// ErrReviewBusy: la fila está reservada por un ApplyApproved en curso ('applying').
	ErrReviewBusy = newErr(CodeReviewBusy)
	// ErrReviewConflict: otra escritura ganó la carrera; se puede reintentar.
	ErrReviewConflict = newErr(CodeReviewConflict)
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
	cas := r.casReviews()
	for _, row := range rows {
		if row.Action != ActionReview {
			continue
		}
		for attempt := 0; ; attempt++ {
			if attempt == casRetries {
				return newErr(CodeReviewConflict, "key", row.Key)
			}
			e := ReviewEntry{
				Org: org, Entity: entity, Key: row.Key, Profile: run.Profile, ProfileVersion: run.ProfileVersion, ProfileHash: run.ProfileHash,
				RunID: run.ID, Row: row.Row, Hash: row.Hash, BaseHash: row.TargetHash, Record: row.Record, Media: row.Media, Items: row.Review, Outcome: row.Outcome,
				State: ReviewPending, CreatedAt: now, UpdatedAt: now,
			}
			prev, ok, err := r.Reviews.Get(ctx, org, entity, row.Key)
			if err != nil {
				return wrapErr(err, CodeReviewStore)
			}
			if ok && prev.Hash == row.Hash && prev.BaseHash == row.TargetHash && prev.State != ReviewApplied && prev.State != ReviewSuperseded {
				// Misma versión de la fila y el destino sigue como cuando se decidió:
				// se conserva la decisión (o la reserva 'applying' en curso).
				e.State, e.DecidedBy, e.DecidedAt, e.Note, e.CreatedAt, e.AppliedRun = prev.State, prev.DecidedBy, prev.DecidedAt, prev.Note, prev.CreatedAt, prev.AppliedRun
			}
			if cas == nil {
				if err := r.Reviews.Put(ctx, e); err != nil {
					return wrapErr(err, CodeReviewStore)
				}
				break
			}
			expect := ReviewExpect{Missing: true}
			if ok {
				expect = expectOf(prev)
			}
			done, err := cas.PutIf(ctx, e, expect)
			if err != nil {
				return wrapErr(err, CodeReviewStore)
			}
			if done {
				break
			}
			// Otro la cambió entre la lectura y la escritura: se vuelve a leer.
		}
	}
	return nil
}

// supersedeReviews pasa a superseded las entradas abiertas (pendientes,
// aprobadas sin aplicar o rechazadas) que esta corrida resolvió sin
// intervención (resolved(key): la trajo y ya no pide revisión). Una aprobada
// que no se superara aquí la escribiría después ApplyApproved encima de la
// versión más nueva que esta corrida acaba de cargar.
func (r *Runner) supersedeReviews(ctx context.Context, org, entity string, resolved func(string) bool) error {
	if r.Reviews == nil {
		return nil
	}
	now := r.now()
	open, err := r.Reviews.List(ctx, org, ReviewFilter{Entity: entity})
	if err != nil {
		return wrapErr(err, CodeReviewStore)
	}
	cas := r.casReviews()
	for _, e := range open {
		switch e.State {
		case ReviewPending, ReviewApproved, ReviewRejected:
		default:
			continue // applying (en curso), applied o superseded: cerradas
		}
		if !resolved(e.Key) {
			continue
		}
		prev := e
		e.State, e.UpdatedAt = ReviewSuperseded, now
		if cas != nil {
			// Si alguien la decidió entre el List y aquí, su decisión no se pisa.
			if _, err := cas.PutIf(ctx, e, expectOf(prev)); err != nil {
				return wrapErr(err, CodeReviewStore)
			}
			continue
		}
		if err := r.Reviews.Put(ctx, e); err != nil {
			return wrapErr(err, CodeReviewStore)
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
//
// Con ConditionalReviewStore cada decisión se escribe con PutIf contra la versión
// leída: una fila que ApplyApproved reservó ('applying') se rechaza con
// CodeReviewBusy, y si otra escritura gana la carrera se deshacen las decisiones
// ya escritas de esta llamada y se devuelve CodeReviewConflict (se puede reintentar).
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
	var out, prevs []ReviewEntry
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
		case e.State == ReviewApplying:
			return nil, newErr(CodeReviewBusy, "key", d.Key)
		case e.Hash != d.Hash:
			return nil, newErr(CodeReviewStale, "key", d.Key)
		}
		prevs = append(prevs, e)
		e.State = ReviewRejected
		if d.Approve {
			e.State = ReviewApproved
		}
		e.DecidedBy, e.DecidedAt, e.Note, e.UpdatedAt = actor, r.now(), d.Note, r.now()
		out = append(out, e)
	}
	cas := r.casReviews()
	if cas == nil {
		for _, e := range out {
			if err := r.Reviews.Put(ctx, e); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	for i, e := range out {
		done, err := cas.PutIf(ctx, e, expectOf(prevs[i]))
		if err == nil && done {
			continue
		}
		// Se deshace lo ya escrito de esta llamada, solo si nadie lo tocó después.
		for j := i - 1; j >= 0; j-- {
			_, _ = cas.PutIf(context.WithoutCancel(ctx), prevs[j], expectOf(out[j]))
		}
		if err != nil {
			return nil, err
		}
		return nil, newErr(CodeReviewConflict, "key", e.Key)
	}
	return out, nil
}

// ApplyApproved escribe en el destino las filas aprobadas de una entidad y deja
// constancia en el historial (ModeReview). Es idempotente: el destino hace upsert
// por identidad y lo ya idéntico no se reescribe. Los medios de la fila se encolan.
//
// Con ConditionalReviewStore cada fila se reserva (approved -> applying) antes de
// escribirla y se marca applied después, ambas con PutIf: una fila escrita nunca
// queda 'rejected'. Si el proceso muere con reservas abiertas, RecoverApplying
// las cierra. Sin la extensión se usa el comportamiento anterior (applyListedBatch),
// que solo acota la carrera con Decide.
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
	run.Progress.Total = len(entries)
	// La corrida queda en el historial desde el inicio (running, con
	// arrendamiento si el ledger lo soporta): se ve en curso, cuenta para la
	// regla de "una aplicación activa" del almacén y, si el proceso muere,
	// Recover la encuentra (la cierra; RecoverApplying cierra sus reservas).
	ctx, save, stop := r.reviewRunSaver(WithRunID(ctx, run.ID), &run)
	defer stop()
	fail := func(err error) (Run, error) {
		run.State, run.FinishedAt = RunFailed, r.now()
		run.setError(err)
		_ = save()
		return run, err
	}
	if err := save(); err != nil {
		return Run{}, err
	}
	cas := r.casReviews()
	for lo := 0; lo < len(entries); lo += r.batch() {
		if err := ctx.Err(); err != nil {
			return fail(contextError(ctx, err))
		}
		batch := entries[lo:min(lo+r.batch(), len(entries))]
		var err error
		if cas != nil {
			err = r.applyReservedBatch(ctx, cas, &run, org, entity, batch)
		} else {
			err = r.applyListedBatch(ctx, &run, org, entity, batch)
		}
		if err != nil {
			return fail(err)
		}
		if err := save(); err != nil {
			return fail(err)
		}
	}
	run.State, run.FinishedAt = RunSucceeded, r.now()
	return run, save()
}

// reviewRunSaver devuelve cómo guardar una corrida apply_reviewed. Con un
// LeaseLedger la guarda con arrendamiento y la mantiene viva con latidos; si
// otro proceso la toma (ErrLeaseLost) se cancela el contexto devuelto. stop
// termina los latidos.
func (r *Runner) reviewRunSaver(ctx context.Context, run *Run) (context.Context, func() error, func()) {
	ll := r.leaser()
	if ll == nil {
		return ctx, func() error { return r.Ledger.Save(context.WithoutCancel(ctx), *run) }, func() {}
	}
	owner := r.owner()
	run.Lease = &RunLease{Owner: owner, Heartbeat: r.now(), Until: r.now().Add(r.leaseTTL())}
	cctx, cancel := context.WithCancelCause(ctx)
	inserted := false
	save := func() error {
		if !inserted {
			// La primera vez se inserta con el arrendamiento ya puesto (como Start).
			inserted = true
			return r.Ledger.Save(context.WithoutCancel(cctx), *run)
		}
		err := ll.SaveLeased(context.WithoutCancel(cctx), *run, owner, r.now(), r.leaseTTL())
		if errors.Is(err, ErrLeaseLost) {
			cancel(ErrLeaseLost)
		}
		return err
	}
	done := make(chan struct{})
	if every := r.heartbeatEvery(); every > 0 {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-cctx.Done():
					return
				case <-t.C:
					if ok, err := ll.Heartbeat(cctx, run.Org, run.ID, owner, r.now(), r.leaseTTL()); err == nil && !ok {
						cancel(ErrLeaseLost)
						return
					}
				}
			}
		}()
	}
	return cctx, save, func() { close(done); cancel(nil) }
}

// destinationMoved dice si el destino cambió para esta identidad después de
// planificar la fila en revisión: otra corrida (o alguien) ya escribió una
// versión distinta de la que vio quien decidió. Escribir lo aprobado ahí
// regresaría el dato a la versión de un archivo anterior.
func destinationMoved(e ReviewEntry, ex Existing, found bool) bool {
	if !found || ex.Hash == e.Hash {
		return false
	}
	if e.Outcome == ActionCreate {
		return true // no existía al planificar y ahora existe con otro contenido
	}
	return e.BaseHash != "" && ex.Hash != e.BaseHash
}

// approvedChanges arma los cambios de un lote contra lo que ya tiene el destino
// y cuenta created/updated/unchanged. Devuelve aparte las entradas cuyo destino
// cambió después de planificarlas (no se escriben).
func approvedChanges(run *Run, batch []ReviewEntry, existing map[string]Existing) (changes []Change, live, moved []ReviewEntry) {
	for _, e := range batch {
		ex, found := existing[e.Key]
		if destinationMoved(e, ex, found) {
			moved = append(moved, e)
			run.Progress.Total--
			run.Progress.Superseded++
			continue
		}
		live = append(live, e)
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
	return changes, live, moved
}

// syncReviewMedia deja al día los medios de filas ya escritas en el destino
// (las vuelve a ligar). Va ANTES de marcarlas applied: si falla, la fila sigue
// sin marcar y el siguiente intento (o RecoverApplying) los encola.
func (r *Runner) syncReviewMedia(ctx context.Context, org, entity, runID string, entries []ReviewEntry) (int, error) {
	if r.Media == nil || len(entries) == 0 {
		return 0, nil
	}
	rows := make([]MediaRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, MediaRow{Key: e.Key, Media: e.Media, Rewritten: true})
	}
	n, err := r.Media.Sync(ctx, org, entity, runID, rows)
	if err != nil {
		return 0, wrapErr(err, CodeMediaEnqueue)
	}
	return n, nil
}

// applyReservedBatch aplica un lote con ConditionalReviewStore. Cada fila se
// reserva (approved -> applying, con su misma huella) ANTES de tocar el destino;
// la que ya no está aprobada con esa huella se salta. Tras escribir, cada
// reserva pasa a applied con otro PutIf. Mientras una fila está en 'applying'
// Decide la rechaza (CodeReviewBusy), así que nunca queda escrita y 'rejected'.
func (r *Runner) applyReservedBatch(ctx context.Context, cas ConditionalReviewStore, run *Run, org, entity string, batch []ReviewEntry) error {
	var reserved []ReviewEntry
	for _, e := range batch {
		res := e
		res.State, res.AppliedRun, res.UpdatedAt = ReviewApplying, run.ID, r.now()
		ok, err := cas.PutIf(ctx, res, ReviewExpect{State: ReviewApproved, Hash: e.Hash})
		if err != nil {
			r.releaseReserved(ctx, cas, org, entity, reserved)
			return wrapErr(err, CodeReviewStore)
		}
		if !ok {
			run.Progress.Total-- // alguien la decidió de nuevo o llegó otra versión
			continue
		}
		reserved = append(reserved, res)
	}
	if len(reserved) == 0 {
		return nil
	}
	keys := make([]string, len(reserved))
	for i, e := range reserved {
		keys[i] = e.Key
	}
	existing, err := r.Target.Lookup(ctx, org, entity, keys)
	if err != nil {
		r.releaseReserved(ctx, cas, org, entity, reserved)
		return wrapErr(err, CodeTargetLookup)
	}
	changes, live, moved := approvedChanges(run, reserved, existing)
	for _, e := range moved {
		sup := e
		sup.State, sup.UpdatedAt = ReviewSuperseded, r.now()
		if _, err := cas.PutIf(ctx, sup, expectOf(e)); err != nil {
			r.releaseReserved(ctx, cas, org, entity, live)
			return wrapErr(err, CodeReviewStore)
		}
	}
	if len(changes) > 0 {
		if err := r.Target.Apply(ctx, org, entity, changes); err != nil {
			// Puede que una parte sí se haya escrito: se resuelve leyendo el destino.
			_, _ = r.settleApplying(context.WithoutCancel(ctx), cas, org, entity, live)
			return wrapErr(err, CodeApplyApproved)
		}
	}
	// Medios antes de marcar applied. Si encolar falla, las filas vuelven a
	// approved: el destino ya las tiene con su huella, así que el siguiente
	// ApplyApproved las ve unchanged (no reescribe nada) y solo encola sus medios.
	n, err := r.syncReviewMedia(ctx, org, entity, run.ID, live)
	if err != nil {
		r.releaseReserved(ctx, cas, org, entity, live)
		return err
	}
	run.Progress.MediaQueued += n
	for _, e := range live {
		done := e
		done.State, done.UpdatedAt = ReviewApplied, r.now()
		ok, err := cas.PutIf(ctx, done, expectOf(e))
		if err != nil {
			return wrapErr(err, CodeReviewStore) // queda en 'applying': RecoverApplying la cierra
		}
		if !ok {
			continue // la tomó RecoverApplying (o la reemplazó una versión nueva): no se pisa
		}
		run.Progress.Done++
	}
	return nil
}

// releaseReserved devuelve a 'approved' reservas de las que no se escribió nada.
func (r *Runner) releaseReserved(ctx context.Context, cas ConditionalReviewStore, org, entity string, reserved []ReviewEntry) {
	ctx = context.WithoutCancel(ctx)
	for _, e := range reserved {
		back := e
		back.State, back.AppliedRun, back.UpdatedAt = ReviewApproved, "", r.now()
		_, _ = cas.PutIf(ctx, back, expectOf(e))
	}
}

// settleApplying deja en un estado seguro filas en 'applying' cuya escritura no
// se sabe si ocurrió: lee el destino y, si ya tiene el registro con la misma
// huella, la fila pasa a applied (y se encolan sus medios); si no, vuelve a
// approved para que un ApplyApproved posterior la escriba. Nunca escribe al
// destino, así que no duplica nada; y como el Target hace upsert por identidad,
// la reescritura posterior de una fila devuelta a approved tampoco duplica.
func (r *Runner) settleApplying(ctx context.Context, cas ConditionalReviewStore, org, entity string, entries []ReviewEntry) (ApplyingReport, error) {
	var rep ApplyingReport
	if len(entries) == 0 {
		return rep, nil
	}
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	existing, err := r.Target.Lookup(ctx, org, entity, keys)
	if err != nil {
		return rep, wrapErr(err, CodeTargetLookup) // siguen en 'applying': se reintenta después
	}
	for _, e := range entries {
		next := e
		next.UpdatedAt = r.now()
		written := false
		if ex, ok := existing[e.Key]; ok && ex.Hash == e.Hash {
			// Escrita: sus medios se encolan antes de marcarla applied, así un
			// fallo al encolar la deja en 'applying' para el siguiente intento.
			n, err := r.syncReviewMedia(ctx, org, entity, e.AppliedRun, []ReviewEntry{e})
			if err != nil {
				return rep, err
			}
			rep.MediaQueued += n
			next.State, written = ReviewApplied, true
		} else {
			next.State, next.AppliedRun = ReviewApproved, ""
		}
		ok, err := cas.PutIf(ctx, next, expectOf(e))
		if err != nil {
			return rep, wrapErr(err, CodeReviewStore)
		}
		if !ok {
			continue
		}
		if !written {
			rep.Released = append(rep.Released, e.Key)
			continue
		}
		rep.Applied = append(rep.Applied, e.Key)
	}
	return rep, nil
}

// ApplyingReport resume lo que RecoverApplying hizo con las filas en 'applying'.
type ApplyingReport struct {
	// Applied son las identidades que el destino ya tenía escritas: pasan a applied.
	Applied []string
	// Released son las que no se alcanzaron a escribir: vuelven a approved.
	Released []string
	// MediaQueued son los medios encolados de las filas que pasaron a applied.
	MediaQueued int
}

// RecoverApplying cierra las filas de una entidad que quedaron en 'applying'
// porque el proceso que las aplicaba murió entre la reserva y la marca final.
// Solo toca las reservas sin cambios desde hace al menos olderThan: debe ser
// mayor que lo que tarda en escribirse un lote, para no tomar las de un
// ApplyApproved que sigue vivo (si lo hiciera, ese proceso aún podría escribir
// una fila que aquí se devolvió a approved). Se llama al arrancar el proceso,
// junto con Recover. Necesita ConditionalReviewStore y el permiso
// ActionReviewApply.
//
// Garantía: no escribe en el destino. Cada fila termina en applied si el destino
// ya tiene el registro con su misma huella, o en approved si no; como el Target
// hace upsert por identidad, el ApplyApproved siguiente reescribe a lo sumo una
// vez la misma fila y nunca crea un duplicado.
func (r *Runner) RecoverApplying(ctx context.Context, org, actor, entity string, olderThan time.Duration) (ApplyingReport, error) {
	if r.Reviews == nil {
		return ApplyingReport{}, newErr(CodeNoReviewStore)
	}
	cas := r.casReviews()
	if cas == nil {
		return ApplyingReport{}, newErr(CodeReviewNoConditional)
	}
	if entity == "" {
		return ApplyingReport{}, newErr(CodeEntityRequired, "action", msg(ActionReviewApply))
	}
	all, err := r.Reviews.List(ctx, org, ReviewFilter{Entity: entity, State: ReviewApplying})
	if err != nil {
		return ApplyingReport{}, wrapErr(err, CodeReviewStore)
	}
	cutoff := r.now().Add(-olderThan)
	var stale []ReviewEntry
	keys := []string{}
	for _, e := range all {
		if !e.UpdatedAt.After(cutoff) {
			stale = append(stale, e)
			keys = append(keys, e.Key)
		}
	}
	if err := r.authorize(ctx, AuthRequest{Org: org, Actor: actor, Action: ActionReviewApply, Entity: entity, Keys: keys}); err != nil {
		return ApplyingReport{}, err
	}
	var rep ApplyingReport
	for lo := 0; lo < len(stale); lo += r.batch() {
		part, err := r.settleApplying(ctx, cas, org, entity, stale[lo:min(lo+r.batch(), len(stale))])
		rep.Applied = append(rep.Applied, part.Applied...)
		rep.Released = append(rep.Released, part.Released...)
		rep.MediaQueued += part.MediaQueued
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// applyListedBatch aplica un lote sin escritura condicional (comportamiento
// anterior): relee el estado antes y después de escribir, lo que acota la
// carrera con Decide pero no la cierra. Una decisión que llega justo entre la
// escritura y la marca 'applied' deja el dato escrito y la entrada 'rejected'.
func (r *Runner) applyListedBatch(ctx context.Context, run *Run, org, entity string, batch []ReviewEntry) error {
	keys := make([]string, len(batch))
	for i, e := range batch {
		keys[i] = e.Key
	}
	existing, err := r.Target.Lookup(ctx, org, entity, keys)
	if err != nil {
		return wrapErr(err, CodeTargetLookup)
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
	changes, live, moved := approvedChanges(run, live, existing)
	for _, e := range moved {
		e.State, e.UpdatedAt = ReviewSuperseded, r.now()
		if err := r.Reviews.Put(ctx, e); err != nil {
			return err
		}
	}
	if len(changes) > 0 {
		if err := r.Target.Apply(ctx, org, entity, changes); err != nil {
			return wrapErr(err, CodeApplyApproved)
		}
	}
	// Medios antes de marcar applied: si encolar falla, la fila sigue aprobada y
	// el siguiente ApplyApproved la reescribe (sin cambios) y los encola.
	n, err := r.syncReviewMedia(ctx, org, entity, run.ID, live)
	if err != nil {
		return err
	}
	run.Progress.MediaQueued += n
	for _, e := range live {
		if !r.stillApproved(ctx, e) {
			continue // cambió durante la escritura: su nuevo estado no se pisa
		}
		e.State, e.AppliedRun, e.UpdatedAt = ReviewApplied, run.ID, r.now()
		if err := r.Reviews.Put(ctx, e); err != nil {
			return err
		}
		run.Progress.Done++
	}
	return nil
}

// stillApproved dice si la entrada sigue aprobada y con la misma huella en el
// almacén. Sin ConditionalReviewStore no hay compare-and-swap, así que esto acota
// la ventana de carrera con Decide y con otra carga, no la cierra del todo.
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

var _ ConditionalReviewStore = (*MemoryReviewStore)(nil)

// PutIf escribe e solo si la entrada guardada coincide con expect (atómico bajo
// el mismo candado que Put y Get).
func (s *MemoryReviewStore) PutIf(_ context.Context, e ReviewEntry, expect ReviewExpect) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := mk(e.Org, e.Entity, e.Key)
	cur, ok := s.m[k]
	switch {
	case expect.Missing && ok:
		return false, nil
	case !expect.Missing && (!ok || cur.State != expect.State || cur.Hash != expect.Hash):
		return false, nil
	}
	s.m[k] = e
	return true, nil
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
