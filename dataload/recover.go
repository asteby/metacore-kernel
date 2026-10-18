package dataload

import (
	"context"
)

// Recuperación de corridas huérfanas. Una corrida en 'running' tiene un
// arrendamiento (lease) a nombre del proceso que la ejecuta, que se renueva con
// latidos (heartbeat) y con cada lote escrito. Si el proceso muere, el
// arrendamiento vence y Recover la toma, reconstruye su solicitud, comprueba que
// perfil y entrada sean EXACTAMENTE los de la corrida y la reanuda.
//
// Reanudar no duplica nada por tres razones: el destino hace upsert por
// identidad; el plan de la reanudación ya ve como 'unchanged' lo que el dueño
// anterior alcanzó a escribir; y el dueño anterior, si en realidad seguía vivo,
// pierde el cerco del ledger (ErrLeaseLost) y se retira sin tocar el historial.

// ErrNoLeaseLedger: Recover necesita un Ledger con arrendamientos.
var ErrNoLeaseLedger = newErr(CodeNoLeaseLedger)

// RequestResolver reconstruye la solicitud de una corrida interrumpida: el
// perfil resuelto (p. ej. con Registry.Resolve), la extracción y la fuente.
// Debe devolver lo mismo que se usó al iniciarla; Recover lo verifica con las
// huellas de la corrida y rechaza reanudar si no coincide.
type RequestResolver func(ctx context.Context, run Run) (RunRequest, error)

// RecoverReport resume una pasada de Recover.
type RecoverReport struct {
	// Resumed son las corridas tomadas y relanzadas (Wait las espera por ID).
	Resumed []Run
	// Failed son las que no se pudieron reanudar y quedaron marcadas como fallidas.
	Failed []Run
}

// Recover toma las corridas de la organización (o de todas, con org "") que
// quedaron en queued/running sin arrendamiento vigente y las reanuda. Se llama al
// arrancar el proceso y, si se quiere, de forma periódica.
func (r *Runner) Recover(ctx context.Context, org string, resolve RequestResolver) (RecoverReport, error) {
	ll := r.leaser()
	if ll == nil {
		return RecoverReport{}, ErrNoLeaseLedger
	}
	if resolve == nil {
		return RecoverReport{}, newErr(CodeRecoverNoResolver)
	}
	claimed, err := ll.ClaimStale(ctx, org, r.now(), r.owner(), r.leaseTTL(), 100)
	if err != nil {
		return RecoverReport{}, err
	}
	var rep RecoverReport
	for _, run := range claimed {
		req, src, err := r.rebuild(ctx, run, resolve)
		if err != nil {
			rep.Failed = append(rep.Failed, r.abandon(ctx, ll, run, err))
			continue
		}
		r.mu.Lock()
		if _, busy := r.inflight[run.Org+"|"+run.Key]; busy {
			r.mu.Unlock()
			continue
		}
		r.spawnLocked(ctx, req, src, run)
		rep.Resumed = append(rep.Resumed, run)
	}
	return rep, nil
}

// rebuild reconstruye la solicitud y comprueba que sea la misma de la corrida.
func (r *Runner) rebuild(ctx context.Context, run Run, resolve RequestResolver) (RunRequest, Source, error) {
	req, err := resolve(ctx, run)
	if err != nil {
		return RunRequest{}, nil, wrapErr(err, CodeRecoverRebuild)
	}
	if req.Org != run.Org {
		return RunRequest{}, nil, newErr(CodeRecoverOrgMismatch, "got", req.Org, "want", run.Org)
	}
	prof, src, in, err := r.prepare(ctx, req)
	if err != nil {
		return RunRequest{}, nil, err
	}
	exHash := ""
	if req.Extract != nil {
		exHash = req.Extract.Hash()
	}
	switch {
	case prof.Hash() != run.ProfileHash || exHash != run.ExtractHash:
		return RunRequest{}, nil, newErr(CodeRecoverProfileChanged)
	case in != run.InputHash:
		return RunRequest{}, nil, newErr(CodeRecoverInputChanged)
	case RunKey(req.Org, fingerprint(prof.Hash(), req.Extract), in, ModeApply) != run.Key:
		return RunRequest{}, nil, newErr(CodeRecoverKeyMismatch)
	}
	return req, src, nil
}

// abandon marca como fallida una corrida que no se puede reanudar, para que no
// quede para siempre en running. Repetir la carga con la misma entrada continúa
// donde quedó porque el diff deja unchanged lo ya escrito.
func (r *Runner) abandon(ctx context.Context, ll LeaseLedger, run Run, cause error) Run {
	run.State, run.FinishedAt = RunFailed, r.now()
	run.setError(wrapErr(cause, CodeRecoverAbandoned))
	_ = ll.SaveLeased(context.WithoutCancel(ctx), run, r.owner(), r.now(), r.leaseTTL())
	return run
}
