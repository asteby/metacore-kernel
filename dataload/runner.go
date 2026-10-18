package dataload

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/importer"
)

// RunRequest describe una corrida.
type RunRequest struct {
	Org       string
	Actor     string
	Profile   Profile
	Overrides *Overrides
	Source    Source
	// Registry, si viene (ResolvedProfile.Request lo llena), liga la corrida con la
	// versión del registro de la que salió el perfil.
	Registry *RegistryRef
	// Extract es la etapa de extracción opcional (ver ParseExtract). Entra en la
	// llave de idempotencia: cambiar el documento produce una corrida distinta.
	Extract *Extractor
	// Force aplica aunque ya exista una corrida exitosa con la misma entrada
	// (por ejemplo tras borrar datos en el destino). Sigue siendo seguro: el
	// diff deja unchanged lo que ya está.
	Force bool
	// FailOnInvalid cancela la aplicación si alguna fila no es válida, en vez de
	// aplicar las válidas y reportar el resto.
	FailOnInvalid bool
}

// Runner orquesta dry-run, aplicación asíncrona, historial y cola de medios.
type Runner struct {
	Ledger Ledger
	Target Target
	// Media es opcional; sin ella los medios del perfil se ignoran con aviso.
	Media *MediaQueue
	// Reviews es opcional; guarda las filas en espera de decisión (ActionReview)
	// para aplicarlas después con Decide y ApplyApproved. Sin él, esas filas solo
	// se reportan en el plan y en la corrida.
	Reviews ReviewStore
	// Authorizer decide quién puede decidir y aplicar filas en revisión (acciones
	// ActionReviewDecide y ActionReviewApply). Sin él esas operaciones se niegan
	// (ErrNoAuthorizer) salvo con AllowAnyActor.
	Authorizer Authorizer
	// AllowAnyActor permite operar sin Authorizer (solo exige org y actor). Es
	// para pruebas y desarrollo; en producción ops debe dar un Authorizer.
	AllowAnyActor bool
	// PageSize: filas por página al planificar una StreamSource; 0 = DefaultPageSize.
	// Se redondea a un múltiplo de BatchSize.
	PageSize int
	// BatchSize de escritura al destino; default 200.
	BatchSize int
	// Now es el reloj; default time.Now.
	Now func() time.Time

	// Owner identifica a este proceso en los arrendamientos de corridas (solo con
	// un LeaseLedger); default un id aleatorio por Runner.
	Owner string
	// LeaseTTL es cuánto vale un arrendamiento sin latido; default 2 minutos.
	LeaseTTL time.Duration
	// HeartbeatEvery es cada cuánto late una corrida en curso; default LeaseTTL/3,
	// negativo = solo late al terminar cada lote.
	HeartbeatEvery time.Duration

	ownerOnce sync.Once
	mu        sync.Mutex
	inflight  map[string]string // org|key -> run id
	cancels   map[string]context.CancelFunc
	done      map[string]chan struct{}
	wg        sync.WaitGroup
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) owner() string {
	r.ownerOnce.Do(func() {
		if r.Owner == "" {
			r.Owner = "runner-" + newID()
		}
	})
	return r.Owner
}

func (r *Runner) leaseTTL() time.Duration {
	if r.LeaseTTL > 0 {
		return r.LeaseTTL
	}
	return 2 * time.Minute
}

func (r *Runner) heartbeatEvery() time.Duration {
	switch {
	case r.HeartbeatEvery < 0:
		return 0
	case r.HeartbeatEvery > 0:
		return r.HeartbeatEvery
	}
	return r.leaseTTL() / 3
}

func (r *Runner) leaser() LeaseLedger {
	ll, _ := r.Ledger.(LeaseLedger)
	return ll
}

func (r *Runner) batch() int {
	if r.BatchSize > 0 {
		return r.BatchSize
	}
	return 200
}

type rowsSource struct {
	name string
	rows []map[string]any
}

func (s rowsSource) Name() string                                   { return s.name }
func (s rowsSource) Rows(context.Context) ([]map[string]any, error) { return s.rows, nil }

// NewRowsSource envuelve filas ya leídas como una Source.
func NewRowsSource(name string, rows []map[string]any) Source { return rowsSource{name, rows} }

func (r *Runner) prepare(ctx context.Context, req RunRequest) (Profile, Source, string, error) {
	if req.Org == "" || req.Source == nil {
		return Profile{}, nil, "", newErr(CodeRequestIncomplete)
	}
	prof, err := req.Profile.Resolve(req.Overrides)
	if err != nil {
		return Profile{}, nil, "", err
	}
	if req.Extract != nil {
		if err := req.Extract.CheckProfile(prof); err != nil {
			return Profile{}, nil, "", err
		}
	}
	// Una StreamSource no se carga: basta su huella (una pasada) para la llave de
	// idempotencia; el plan la vuelve a recorrer.
	if ss, ok := req.Source.(StreamSource); ok {
		in, _, err := scanInput(ctx, ss, prof.maxStreamRows())
		if err != nil {
			return Profile{}, nil, "", err
		}
		return prof, ss, in, nil
	}
	rows, err := req.Source.Rows(ctx)
	if err != nil {
		return Profile{}, nil, "", wrapErr(err, CodeSourceRead, "source", req.Source.Name())
	}
	return prof, rowsSource{req.Source.Name(), rows}, inputHash(rows), nil
}

func (r *Runner) newRun(req RunRequest, prof Profile, src Source, in string, mode RunMode) Run {
	run := Run{
		ID: newID(), Key: RunKey(req.Org, fingerprint(prof.Hash(), req.Extract), in, mode), Org: req.Org, Mode: mode, State: RunQueued,
		Actor: req.Actor, Profile: prof.Name, ProfileVersion: prof.Version, ProfileHash: prof.Hash(),
		OverridesHash: req.Overrides.Hash(), Source: src.Name(), InputHash: in, CreatedAt: r.now(),
	}
	if req.Extract != nil {
		run.ExtractHash = req.Extract.Hash()
	}
	if req.Registry != nil {
		run.Registry = req.Registry
	}
	return run
}

// DryRun planifica sin escribir nada y lo deja en el historial. Devuelve el
// plan completo (con el diff por fila).
func (r *Runner) DryRun(ctx context.Context, req RunRequest) (*Plan, Run, error) {
	prof, src, in, err := r.prepare(ctx, req)
	if err != nil {
		return nil, Run{}, err
	}
	run := r.newRun(req, prof, src, in, ModeDryRun)
	run.State, run.StartedAt = RunRunning, r.now()
	var plan *Plan
	if _, ok := src.(StreamSource); ok {
		plan, err = r.dryRunStream(ctx, RunRequest{Org: req.Org, Profile: req.Profile, Overrides: req.Overrides, Source: src, Extract: req.Extract}, &run)
		if err == nil && plan.InputHash != in {
			plan, err = nil, newErr(CodeSourceChanged, "source", src.Name())
		}
	} else {
		plan, err = BuildPlan(ctx, PlanRequest{Org: req.Org, Profile: req.Profile, Overrides: req.Overrides, Source: src, Target: r.Target, Extract: req.Extract})
	}
	run.FinishedAt = r.now()
	if err != nil {
		run.State = RunFailed
		run.setError(err)
		_ = r.Ledger.Save(ctx, run)
		return nil, run, err
	}
	run.State, run.PlanHash, run.Progress = RunSucceeded, plan.PlanHash, plan.progress()
	if plan.Rows != nil {
		run.Issues, run.Review = plan.issues(), plan.reviewNotes()
	}
	return plan, run, r.Ledger.Save(ctx, run)
}

// Start encola una aplicación y vuelve de inmediato; el avance se consulta con
// Status/Wait. Si la misma entrada ya se aplicó con éxito (y no hay Force)
// registra una corrida replay que no escribe nada. Si la misma entrada ya está
// en curso (en este proceso o, con un LeaseLedger, en otro) devuelve esa corrida.
func (r *Runner) Start(ctx context.Context, req RunRequest) (Run, error) {
	prof, src, in, err := r.prepare(ctx, req)
	if err != nil {
		return Run{}, err
	}
	run := r.newRun(req, prof, src, in, ModeApply)
	flight := req.Org + "|" + run.Key
	ll := r.leaser()
	if ll != nil {
		run.Lease = &RunLease{Owner: r.owner(), Heartbeat: r.now(), Until: r.now().Add(r.leaseTTL())}
	}

	r.mu.Lock()
	if id, ok := r.inflight[flight]; ok {
		r.mu.Unlock()
		return r.Ledger.Get(ctx, req.Org, id)
	}
	if ll != nil {
		if active, found, err := ll.FindActive(ctx, req.Org, run.Key, r.now()); err != nil {
			r.mu.Unlock()
			return Run{}, err
		} else if found {
			r.mu.Unlock()
			return active, nil
		}
	}
	if !req.Force {
		prev, found, err := r.Ledger.FindApplied(ctx, req.Org, run.Key)
		if err != nil {
			r.mu.Unlock()
			return Run{}, err
		}
		if found {
			r.mu.Unlock()
			run.Lease = nil
			run.State, run.ReplayOf, run.PlanHash, run.Progress = RunSucceeded, prev.ID, prev.PlanHash, prev.Progress
			run.StartedAt, run.FinishedAt = r.now(), r.now()
			return run, r.Ledger.Save(ctx, run)
		}
	}
	if err := r.Ledger.Save(ctx, run); err != nil {
		r.mu.Unlock()
		return Run{}, err
	}
	r.spawnLocked(ctx, req, src, run) // suelta r.mu
	return run, nil
}

// spawnLocked registra la corrida como en curso y arranca su trabajo. Requiere r.mu
// tomado y lo suelta.
func (r *Runner) spawnLocked(ctx context.Context, req RunRequest, src Source, run Run) {
	flight := req.Org + "|" + run.Key
	if r.inflight == nil {
		r.inflight, r.cancels, r.done = map[string]string{}, map[string]context.CancelFunc{}, map[string]chan struct{}{}
	}
	// El trabajo sobrevive a la petición que lo inició; se corta con Cancel/Close.
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	r.inflight[flight], r.cancels[run.ID], r.done[run.ID] = run.ID, cancel, done
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer close(done)
		defer cancel()
		r.execute(wctx, req, src, run)
		r.mu.Lock()
		delete(r.inflight, flight)
		delete(r.cancels, run.ID)
		delete(r.done, run.ID)
		r.mu.Unlock()
	}()
}

func (r *Runner) execute(ctx context.Context, req RunRequest, src Source, run Run) {
	ll := r.leaser()
	owner := r.owner()
	ctx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	lost := func() bool { return errors.Is(context.Cause(ctx), ErrLeaseLost) }

	// save escribe la corrida. Con un LeaseLedger lo hace con cerco: si otro
	// proceso recuperó la corrida, se suelta sin tocar su historial.
	save := func() bool {
		sctx := context.WithoutCancel(ctx)
		if ll == nil {
			_ = r.Ledger.Save(sctx, run)
			return true
		}
		if err := ll.SaveLeased(sctx, run, owner, r.now(), r.leaseTTL()); errors.Is(err, ErrLeaseLost) {
			cancelCause(ErrLeaseLost)
			return false
		}
		return true
	}
	fail := func(err error) {
		if lost() {
			return
		}
		run.FinishedAt = r.now()
		if errors.Is(err, context.Canceled) {
			run.State = RunCanceled
		} else {
			run.State = RunFailed
		}
		run.setError(err)
		save()
	}
	if ll != nil {
		if every := r.heartbeatEvery(); every > 0 {
			go func() {
				t := time.NewTicker(every)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						if ok, err := ll.Heartbeat(ctx, run.Org, run.ID, owner, r.now(), r.leaseTTL()); err == nil && !ok {
							cancelCause(ErrLeaseLost)
							return
						}
					}
				}
			}()
		}
	}

	// Una corrida recuperada trae el avance que dejó su dueño anterior.
	base := run.Progress
	marker, _ := r.Target.(MarkingTarget)
	run.State, run.StartedAt = RunRunning, r.now()
	if !save() {
		return
	}

	stop := func(err error) {
		if !errors.Is(err, errStopped) {
			fail(err)
		}
	}
	if ss, ok := src.(StreamSource); ok {
		if err := r.executeStream(ctx, req, ss, &run, base, marker, save); err != nil {
			stop(err)
			return
		}
		run.State, run.FinishedAt = RunSucceeded, r.now()
		save()
		return
	}

	plan, err := BuildPlan(ctx, PlanRequest{Org: req.Org, Profile: req.Profile, Overrides: req.Overrides, Source: src, Target: r.Target, Extract: req.Extract})
	if err != nil {
		fail(err)
		return
	}
	run.PlanHash, run.Progress, run.Issues, run.Review = plan.PlanHash, plan.progress(), plan.issues(), plan.reviewNotes()
	if err := r.holdReviews(ctx, run, plan); err != nil {
		fail(err)
		return
	}
	if req.FailOnInvalid && (run.Progress.Invalid > 0 || run.Progress.Duplicates > 0) {
		fail(newErr(CodeRunInvalidRows, "invalid", run.Progress.Invalid, "duplicates", run.Progress.Duplicates))
		return
	}
	w, err := r.newBatchWriter(ctx, req.Org, plan.Entity, &run, &base, marker, save)
	if err != nil {
		fail(err)
		return
	}
	// Reanudación: lo que escribió el dueño anterior ya aparece como unchanged en
	// este plan; se suma al avance y se quita de unchanged.
	if base.Done > 0 {
		run.Progress.Total += base.Done
		run.Progress.Unchanged = max(0, run.Progress.Unchanged-base.Done)
	}
	run.Progress.Done, run.Progress.Created, run.Progress.Updated = base.Done, base.Created, base.Updated
	if !save() {
		return
	}
	if err := w.write(plan.Changes()); err != nil {
		stop(err)
		return
	}
	if err := ctx.Err(); err != nil {
		fail(contextError(ctx, err))
		return
	}

	// Medios de TODAS las filas válidas, también las unchanged: encolar es
	// idempotente y así una corrida interrumpida entre escribir y encolar se
	// recupera sola. Las filas en revisión no encolan nada hasta que se aprueben.
	n, err := r.enqueueMedia(ctx, req.Org, plan.Entity, run.ID, plan.Rows, nil)
	if err != nil {
		fail(err)
		return
	}
	run.Progress.MediaQueued = n
	run.State, run.FinishedAt = RunSucceeded, r.now()
	save()
}

// errStopped: la corrida se soltó porque otro proceso la tomó (ya no se escribe
// nada). Es interno: nunca llega a quien llama.
var errStopped error = internalErr("stopped")

// internalErr es una señal interna del motor, nunca un mensaje para personas.
type internalErr string

func (e internalErr) Error() string { return "dataload: " + string(e) }

// batchWriter escribe cambios al destino por lotes, con marca de aplicación (si
// el destino la soporta) y checkpoint después de cada lote.
type batchWriter struct {
	r       *Runner
	ctx     context.Context
	org     string
	entity  string
	run     *Run
	cp      Checkpoint
	marker  MarkingTarget
	save    func() bool
	written int // cambios escritos por esta ejecución (para los mensajes de lote)
}

// newBatchWriter prepara el checkpoint de la corrida y, si es una reanudación con
// marcas, suma a base lo que el dueño anterior escribió después de su último
// checkpoint.
func (r *Runner) newBatchWriter(ctx context.Context, org, entity string, run *Run, base *Progress, marker MarkingTarget, save func() bool) (*batchWriter, error) {
	w := &batchWriter{r: r, ctx: ctx, org: org, entity: entity, run: run, marker: marker, save: save}
	if run.Checkpoint != nil {
		w.cp = *run.Checkpoint
	}
	if marker != nil && run.Checkpoint != nil {
		marks, err := marker.ApplyMarks(ctx, org, run.ID, w.cp.Batches)
		if err != nil {
			return nil, wrapErr(err, CodeTargetMarks)
		}
		for _, m := range latestMarks(marks) {
			n := m.Created + m.Updated
			base.Done, base.Created, base.Updated = base.Done+n, base.Created+m.Created, base.Updated+m.Updated
			w.cp.Batches, w.cp.Rows, w.cp.LastKey, w.cp.Recovered = m.Batch, w.cp.Rows+n, m.LastKey, w.cp.Recovered+n
		}
		// El checkpoint avanza junto con el avance que ya incluye esas marcas: si
		// esta ejecución también muere antes de su primer lote, la siguiente no
		// las vuelve a sumar.
		saved := w.cp
		run.Checkpoint = &saved
	}
	return w, nil
}

// write aplica changes en lotes de BatchSize. errStopped si la corrida se soltó.
func (w *batchWriter) write(changes []Change) error {
	r, run := w.r, w.run
	for lo := 0; lo < len(changes); lo += r.batch() {
		if err := w.ctx.Err(); err != nil {
			return contextError(w.ctx, err)
		}
		hi := min(lo+r.batch(), len(changes))
		mark := ApplyMark{RunID: run.ID, Batch: w.cp.Batches + 1, Generation: w.cp.Resumes, LastKey: changes[hi-1].Key}
		for _, c := range changes[lo:hi] {
			if c.Action == ActionCreate {
				mark.Created++
			} else {
				mark.Updated++
			}
		}
		var err error
		if w.marker != nil {
			err = w.marker.ApplyMarked(w.ctx, w.org, w.entity, mark, changes[lo:hi])
		} else {
			err = r.Target.Apply(w.ctx, w.org, w.entity, changes[lo:hi])
		}
		if err != nil {
			return wrapErr(err, CodeApplyBatch, "from", w.written+lo+1, "to", w.written+hi)
		}
		run.Progress.Done += hi - lo
		run.Progress.Created += mark.Created
		run.Progress.Updated += mark.Updated
		w.cp.Batches++
		w.cp.Rows, w.cp.LastKey, w.cp.UpdatedAt = run.Progress.Done, changes[hi-1].Key, r.now()
		saved := w.cp
		run.Checkpoint = &saved
		if !w.save() {
			return errStopped
		}
	}
	w.written += len(changes)
	return nil
}

// enqueueMedia deja vigentes los medios de las filas válidas que no esperan
// revisión (only, si no es nil, acota cuáles): encola lo que falte y vuelve a
// ligar lo necesario (ver MediaQueue.Sync). Devuelve cuántos trabajos eran nuevos.
func (r *Runner) enqueueMedia(ctx context.Context, org, entity, runID string, rows []RowPlan, only func(RowPlan) bool) (int, error) {
	if r.Media == nil {
		return 0, nil
	}
	var mrows []MediaRow
	for _, row := range rows {
		if row.Action == ActionInvalid || row.Action == ActionDuplicate || row.Action == ActionReview || (only != nil && !only(row)) {
			continue
		}
		mrows = append(mrows, MediaRow{Key: row.Key, Media: row.Media, Rewritten: row.Action == ActionCreate || row.Action == ActionUpdate})
	}
	if len(mrows) == 0 {
		return 0, nil
	}
	n, err := r.Media.Sync(ctx, org, entity, runID, mrows)
	if err != nil {
		return 0, wrapErr(err, CodeMediaEnqueue)
	}
	return n, nil
}

// contextError devuelve la causa de cancelación si hay una (p. ej. ErrLeaseLost).
func contextError(ctx context.Context, err error) error {
	if c := context.Cause(ctx); c != nil {
		return c
	}
	return err
}

func (p *Plan) progress() Progress {
	return Progress{
		Total: p.Counts[ActionCreate] + p.Counts[ActionUpdate], Unchanged: p.Counts[ActionUnchanged],
		Invalid: p.Counts[ActionInvalid], Duplicates: p.Counts[ActionDuplicate],
		Created: p.Counts[ActionCreate], Updated: p.Counts[ActionUpdate], Review: p.Counts[ActionReview],
	}
}

func (p *Plan) issues() []importer.RowIssue {
	var out []importer.RowIssue
	for _, r := range p.Rows {
		for _, is := range r.Issues {
			if len(out) == MaxRunIssues {
				return out
			}
			out = append(out, is)
		}
	}
	return out
}

// Status devuelve el estado y progreso actuales de una corrida.
func (r *Runner) Status(ctx context.Context, org, id string) (Run, error) {
	return r.Ledger.Get(ctx, org, id)
}

// Wait bloquea hasta que la corrida termine (o ctx expire) y devuelve su estado final.
func (r *Runner) Wait(ctx context.Context, org, id string) (Run, error) {
	r.mu.Lock()
	ch := r.done[id]
	r.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return Run{}, ctx.Err()
		}
	}
	return r.Ledger.Get(ctx, org, id)
}

// Cancel pide detener una corrida en curso. Los lotes ya escritos se conservan;
// repetir la corrida continúa donde quedó porque el diff deja unchanged lo escrito.
func (r *Runner) Cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cancels[id]; ok {
		c()
		return true
	}
	return false
}

// Close cancela todo lo que esté corriendo y espera a que termine.
func (r *Runner) Close() {
	r.mu.Lock()
	for _, c := range r.cancels {
		c()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// setError deja el fallo en la corrida: texto por omisión y, si viene del motor,
// su código y parámetros.
func (run *Run) setError(err error) {
	run.Error = err.Error()
	run.ErrorCode, run.ErrorParams = "", nil
	if m, ok := ErrorMessage(err); ok {
		run.ErrorCode, run.ErrorParams = m.Code, m.Params
	}
}

// latestMarks deja, por número de lote, la marca de la generación más reciente:
// si un dueño anterior colgado despierta y escribe tarde un lote con el mismo
// número que ya escribió quien lo recuperó, cuenta el de este último.
func latestMarks(marks []ApplyMark) []ApplyMark {
	byBatch := map[int]ApplyMark{}
	for _, m := range marks {
		if prev, ok := byBatch[m.Batch]; !ok || m.Generation > prev.Generation {
			byBatch[m.Batch] = m
		}
	}
	out := make([]ApplyMark, 0, len(byBatch))
	for _, m := range byBatch {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Batch < out[j].Batch })
	return out
}
