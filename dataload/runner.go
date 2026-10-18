package dataload

import (
	"context"
	"errors"
	"fmt"
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
	// BatchSize de escritura al destino; default 200.
	BatchSize int
	// Now es el reloj; default time.Now.
	Now func() time.Time

	mu       sync.Mutex
	inflight map[string]string // org|key -> run id
	cancels  map[string]context.CancelFunc
	done     map[string]chan struct{}
	wg       sync.WaitGroup
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
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
		return Profile{}, nil, "", errors.New("dataload: faltan org o source")
	}
	prof, err := req.Profile.Resolve(req.Overrides)
	if err != nil {
		return Profile{}, nil, "", err
	}
	rows, err := req.Source.Rows(ctx)
	if err != nil {
		return Profile{}, nil, "", fmt.Errorf("dataload: no se pudo leer %s: %w", req.Source.Name(), err)
	}
	return prof, rowsSource{req.Source.Name(), rows}, inputHash(rows), nil
}

func (r *Runner) newRun(req RunRequest, prof Profile, src Source, in string, mode RunMode) Run {
	return Run{
		ID: newID(), Key: RunKey(req.Org, prof.Hash(), in, mode), Org: req.Org, Mode: mode, State: RunQueued,
		Actor: req.Actor, Profile: prof.Name, ProfileVersion: prof.Version, ProfileHash: prof.Hash(),
		OverridesHash: req.Overrides.Hash(), Source: src.Name(), InputHash: in, CreatedAt: r.now(),
	}
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
	plan, err := BuildPlan(ctx, PlanRequest{Org: req.Org, Profile: req.Profile, Overrides: req.Overrides, Source: src, Target: r.Target})
	run.FinishedAt = r.now()
	if err != nil {
		run.State, run.Error = RunFailed, err.Error()
		_ = r.Ledger.Save(ctx, run)
		return nil, run, err
	}
	run.State, run.PlanHash, run.Progress, run.Issues = RunSucceeded, plan.PlanHash, plan.progress(), plan.issues()
	return plan, run, r.Ledger.Save(ctx, run)
}

// Start encola una aplicación y vuelve de inmediato; el avance se consulta con
// Status/Wait. Si la misma entrada ya se aplicó con éxito (y no hay Force)
// registra una corrida replay que no escribe nada. Si la misma entrada ya está
// en curso devuelve esa corrida.
func (r *Runner) Start(ctx context.Context, req RunRequest) (Run, error) {
	prof, src, in, err := r.prepare(ctx, req)
	if err != nil {
		return Run{}, err
	}
	run := r.newRun(req, prof, src, in, ModeApply)
	flight := req.Org + "|" + run.Key

	r.mu.Lock()
	if id, ok := r.inflight[flight]; ok {
		r.mu.Unlock()
		return r.Ledger.Get(ctx, req.Org, id)
	}
	if !req.Force {
		prev, found, err := r.Ledger.FindApplied(ctx, req.Org, run.Key)
		if err != nil {
			r.mu.Unlock()
			return Run{}, err
		}
		if found {
			r.mu.Unlock()
			run.State, run.ReplayOf, run.PlanHash, run.Progress = RunSucceeded, prev.ID, prev.PlanHash, prev.Progress
			run.StartedAt, run.FinishedAt = r.now(), r.now()
			return run, r.Ledger.Save(ctx, run)
		}
	}
	if r.inflight == nil {
		r.inflight, r.cancels, r.done = map[string]string{}, map[string]context.CancelFunc{}, map[string]chan struct{}{}
	}
	if err := r.Ledger.Save(ctx, run); err != nil {
		r.mu.Unlock()
		return Run{}, err
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
	return run, nil
}

func (r *Runner) execute(ctx context.Context, req RunRequest, src Source, run Run) {
	save := func() { _ = r.Ledger.Save(context.WithoutCancel(ctx), run) }
	fail := func(err error) {
		run.FinishedAt = r.now()
		if errors.Is(err, context.Canceled) {
			run.State = RunCanceled
		} else {
			run.State = RunFailed
		}
		run.Error = err.Error()
		save()
	}
	run.State, run.StartedAt = RunRunning, r.now()
	save()

	plan, err := BuildPlan(ctx, PlanRequest{Org: req.Org, Profile: req.Profile, Overrides: req.Overrides, Source: src, Target: r.Target})
	if err != nil {
		fail(err)
		return
	}
	run.PlanHash, run.Progress, run.Issues = plan.PlanHash, plan.progress(), plan.issues()
	if req.FailOnInvalid && (run.Progress.Invalid > 0 || run.Progress.Duplicates > 0) {
		fail(fmt.Errorf("dataload: %d filas inválidas y %d duplicadas; no se aplicó nada", run.Progress.Invalid, run.Progress.Duplicates))
		return
	}
	run.Progress.Done, run.Progress.Created, run.Progress.Updated = 0, 0, 0
	save()

	changes := plan.Changes()
	for lo := 0; lo < len(changes); lo += r.batch() {
		if err := ctx.Err(); err != nil {
			fail(err)
			return
		}
		hi := min(lo+r.batch(), len(changes))
		if err := r.Target.Apply(ctx, req.Org, plan.Entity, changes[lo:hi]); err != nil {
			fail(fmt.Errorf("dataload: aplicar lote %d-%d: %w", lo+1, hi, err))
			return
		}
		for _, c := range changes[lo:hi] {
			run.Progress.Done++
			if c.Action == ActionCreate {
				run.Progress.Created++
			} else {
				run.Progress.Updated++
			}
		}
		save()
	}

	if err := ctx.Err(); err != nil {
		fail(err)
		return
	}

	// Medios de TODAS las filas válidas, también las unchanged: encolar es
	// idempotente y así una corrida interrumpida entre escribir y encolar se
	// recupera sola.
	if r.Media != nil {
		var jobs []MediaJob
		for _, row := range plan.Rows {
			if row.Action == ActionInvalid || row.Action == ActionDuplicate {
				continue
			}
			jobs = append(jobs, r.Media.jobsFor(req.Org, plan.Entity, run.ID, row.Key, row.Media)...)
		}
		if len(jobs) > 0 {
			n, err := r.Media.Enqueue(ctx, jobs)
			if err != nil {
				fail(fmt.Errorf("dataload: encolar medios: %w", err))
				return
			}
			run.Progress.MediaQueued = n
		}
	}
	run.State, run.FinishedAt = RunSucceeded, r.now()
	save()
}

func (p *Plan) progress() Progress {
	return Progress{
		Total: p.Counts[ActionCreate] + p.Counts[ActionUpdate], Unchanged: p.Counts[ActionUnchanged],
		Invalid: p.Counts[ActionInvalid], Duplicates: p.Counts[ActionDuplicate],
		Created: p.Counts[ActionCreate], Updated: p.Counts[ActionUpdate],
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
