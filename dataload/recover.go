package dataload

import (
	"context"
	"errors"
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
	// Deferred son las que no se pudieron reanudar por una falla transitoria:
	// siguen en running con el arrendamiento vencido y la siguiente pasada de
	// Recover las vuelve a intentar.
	Deferred []Run
}

// ErrNotResumable marca un error definitivo al reconstruir una corrida (la
// solicitud guardada no existe, quien la lanzó ya no tiene permiso, el perfil o
// la entrada cambiaron...). Recover la cierra como fallida. Cualquier otro
// error del RequestResolver se trata como transitorio: la corrida se suelta y se
// reintenta en la siguiente pasada. Un RequestResolver lo usa con
// fmt.Errorf("...: %w", dataload.ErrNotResumable) o NotResumable(err).
var ErrNotResumable = newErr(CodeRecoverNotResumable)

// NotResumable envuelve err como definitivo para Recover.
func NotResumable(err error) error {
	if err == nil {
		return nil
	}
	return notResumable{err}
}

type notResumable struct{ err error }

func (e notResumable) Error() string   { return e.err.Error() }
func (e notResumable) Unwrap() []error { return []error{e.err, ErrNotResumable} }

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
		if run.Mode == ModeReview {
			// Una aplicación de revisadas no se reanuda desde aquí: sus filas
			// quedaron reservadas ('applying') y RecoverApplying las cierra
			// leyendo el destino. La corrida se cierra como fallida.
			rep.Failed = append(rep.Failed, r.abandon(ctx, ll, run, newErr(CodeRecoverReviewRun)))
			continue
		}
		req, src, err := r.rebuild(ctx, run, resolve)
		if err != nil && !errors.Is(err, ErrNotResumable) && ctx.Err() == nil {
			// Falla transitoria (base caída, tiempo agotado...): se suelta el
			// arrendamiento sin pasar a terminal y la siguiente pasada reintenta.
			rep.Deferred = append(rep.Deferred, r.release(ctx, ll, run, err))
			continue
		}
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
		return RunRequest{}, nil, NotResumable(newErr(CodeRecoverOrgMismatch, "got", req.Org, "want", run.Org))
	}
	prof, src, in, err := r.prepare(ctx, req)
	if err != nil {
		if ErrorCode(err) == CodeSourceRead {
			return RunRequest{}, nil, err // leer la fuente puede fallar de paso
		}
		return RunRequest{}, nil, NotResumable(err) // perfil o extracción inválidos
	}
	exHash := ""
	if req.Extract != nil {
		exHash = req.Extract.Hash()
	}
	switch {
	case prof.Hash() != run.ProfileHash || exHash != run.ExtractHash:
		return RunRequest{}, nil, NotResumable(newErr(CodeRecoverProfileChanged))
	case in != run.InputHash:
		return RunRequest{}, nil, NotResumable(newErr(CodeRecoverInputChanged))
	case RunKey(req.Org, fingerprint(prof.Hash(), req.Extract), in, ModeApply) != run.Key:
		return RunRequest{}, nil, NotResumable(newErr(CodeRecoverKeyMismatch))
	}
	return req, src, nil
}

// release suelta el arrendamiento de una corrida que no se pudo reconstruir por
// una falla transitoria: queda en running con el arrendamiento ya vencido (y el
// motivo en Error) para que la siguiente pasada de Recover la vuelva a tomar.
func (r *Runner) release(ctx context.Context, ll LeaseLedger, run Run, cause error) Run {
	run.setError(wrapErr(cause, CodeRecoverDeferred))
	_ = ll.SaveLeased(context.WithoutCancel(ctx), run, r.owner(), r.now(), 0)
	return run
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
