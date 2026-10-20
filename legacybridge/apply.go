package legacybridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/dataload"
)

// Outcome es lo que pasó con un envelope.
type Outcome string

const (
	OutcomeApplied   Outcome = "applied"
	OutcomeDeleted   Outcome = "deleted"
	OutcomeDuplicate Outcome = "duplicate" // misma versión o misma huella
	OutcomeConfirmed Outcome = "confirmed" // eco de un comando del host: solo se confirma el mapeo
	OutcomeStale     Outcome = "stale"     // versión más vieja que la guardada
	OutcomeWaiting   Outcome = "waiting"   // faltan dependencias; se reintenta
	OutcomeConflict  Outcome = "conflict"  // el alcance es del host: va a revisión
	OutcomeReview    Outcome = "review"    // inválido o dependencias vencidas: va a revisión
	OutcomeError     Outcome = "error"     // falla de infraestructura: el lote no avanza el cursor
)

// Códigos de revisión.
const (
	ReviewCodeOwnerConflict     = "legacybridge.owner_conflict"
	ReviewCodeDependencyMissing = "legacybridge.dependency_missing"
	ReviewCodeInvalid           = "legacybridge.invalid_envelope"
	ReviewCodeRefConflict       = "legacybridge.ref_conflict"
	ReviewProfile               = "legacybridge"
)

type invalidErr struct{ err error }

func (e invalidErr) Error() string { return e.err.Error() }
func (e invalidErr) Unwrap() error { return e.err }

// Invalid marca un error del Sink que es del dato y no de la infraestructura
// (una columna que no se puede convertir, una FK que no es un id externo, una
// llave única que choca): el envelope va a revisión y el lote sigue, en lugar
// de detener el cursor reintentando para siempre una fila envenenada.
func Invalid(err error) error {
	if err == nil {
		return nil
	}
	return invalidErr{err}
}

// IsInvalid reporta si err se marcó con Invalid.
func IsInvalid(err error) bool {
	var e invalidErr
	return errors.As(err, &e)
}

// Write es lo que el host escribe por un envelope ya resuelto.
type Write struct {
	Org      string
	System   string
	Envelope Envelope
	// LocalID de la raíz; FacetIDs[i] es el de Envelope.Facets[i].
	LocalID  string
	FacetIDs []string
	// Created: la raíz no tenía mapeo previo en bridge_refs.
	Created bool
}

// Sink es el destino del host. Corre dentro de Config.Atomic: usa el ctx que
// recibe (DB(ctx, …) devuelve la transacción).
type Sink interface {
	// DeriveID da el id local de un external_id sin mapeo (p. ej. un uuidv5
	// determinista, el mismo que usa la carga inicial).
	DeriveID(org, entity, externalID string) string
	// Exists reporta si la fila local existe (resuelve dependencias que se
	// cargaron sin bridge_refs, p. ej. por la carga inicial).
	Exists(ctx context.Context, org, entity, localID string) (bool, error)
	// Upsert escribe la réplica en crudo (sin efectos: no mueve stock, no timbra)
	// y reemplaza el conjunto de hijos de cada clave de Aggregate.Children.
	Upsert(ctx context.Context, w Write) error
	// Delete hace borrado lógico de la raíz (y sus facetas).
	Delete(ctx context.Context, w Write) error
}

// Config arma un Applier.
type Config struct {
	Store Store
	Sink  Sink
	// Review recibe conflictos, inválidos y dependencias vencidas. Opcional: sin
	// él se registran en el log y el envelope se descarta.
	Review dataload.ReviewStore
	// Atomic corre fn en una transacción (ver GormAtomic). Sin él, sin transacción.
	Atomic func(ctx context.Context, fn func(ctx context.Context) error) error
	// DefaultOwner sin regla de propiedad (por omisión legacy: en F0 todo es del legado).
	DefaultOwner Owner
	// BranchEntity es la entidad con que las sucursales están en bridge_refs
	// (para traducir Envelope.BranchExternalID al id local). Vacío: la sucursal
	// del envelope se usa tal cual en el mapa de propiedad.
	BranchEntity string
	// MaxWait es cuánto espera un envelope sus dependencias antes de ir a
	// revisión (24 h por omisión).
	MaxWait time.Duration
	// Backoff del reintento de envelopes en espera (exponencial 30 s .. 1 h).
	Backoff func(attempt int) time.Duration
	Logger  *slog.Logger
	Now     func() time.Time
}

// Applier aplica envelopes del legado en el host.
type Applier struct {
	cfg   Config
	locks keyLocks
}

// NewApplier valida la configuración.
func NewApplier(cfg Config) (*Applier, error) {
	if cfg.Store == nil || cfg.Sink == nil {
		return nil, errors.New("legacybridge: NewApplier requiere Store y Sink")
	}
	if cfg.DefaultOwner == "" {
		cfg.DefaultOwner = OwnerLegacy
	}
	if !cfg.DefaultOwner.Valid() {
		return nil, fmt.Errorf("legacybridge: DefaultOwner %q inválido", cfg.DefaultOwner)
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 24 * time.Hour
	}
	if cfg.Backoff == nil {
		cfg.Backoff = Exponential(30*time.Second, time.Hour)
	}
	if cfg.Atomic == nil {
		cfg.Atomic = func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Logger = cfg.Logger.With("component", "legacybridge")
	return &Applier{cfg: cfg}, nil
}

// Result es el resultado de un envelope.
type Result struct {
	Entity     string  `json:"entity"`
	ExternalID string  `json:"external_id"`
	Outcome    Outcome `json:"outcome"`
	LocalID    string  `json:"local_id,omitempty"`
	Detail     string  `json:"detail,omitempty"`
}

// IngestResult es la respuesta a un lote.
type IngestResult struct {
	Results []Result        `json:"results"`
	Counts  map[Outcome]int `json:"counts"`
	// Cursor es la marca de agua guardada (la del lote si avanzó, si no la anterior).
	Cursor   string `json:"cursor"`
	Advanced bool   `json:"advanced"`
}

const defaultStream = "default"

// Ingest aplica un lote en orden y, si ningún envelope falló por
// infraestructura, guarda b.Cursor. Es seguro reintentar el mismo lote.
func (a *Applier) Ingest(ctx context.Context, org string, b Batch) (IngestResult, error) {
	if b.Source == "" {
		return IngestResult{}, errors.New("legacybridge: lote sin source")
	}
	if len(b.Envelopes) > MaxBatchEnvelopes {
		return IngestResult{}, fmt.Errorf("legacybridge: el lote trae %d envelopes (máximo %d)", len(b.Envelopes), MaxBatchEnvelopes)
	}
	if len(b.Cursor) > maxCursorLen {
		return IngestResult{}, fmt.Errorf("legacybridge: cursor de %d bytes (máximo %d)", len(b.Cursor), maxCursorLen)
	}
	stream := b.Stream
	if stream == "" {
		stream = defaultStream
	}
	if len(stream) > maxIDLen {
		return IngestResult{}, fmt.Errorf("legacybridge: stream de %d bytes (máximo %d)", len(stream), maxIDLen)
	}
	out := IngestResult{Counts: map[Outcome]int{}}
	failed := false
	for _, env := range b.Envelopes {
		if env.Source == "" {
			env.Source = b.Source
		}
		var r Result
		if env.Source != b.Source {
			r = a.review(ctx, org, b.Source, env, ReviewCodeInvalid, fmt.Sprintf("source %q no coincide con el del lote %q", env.Source, b.Source))
		} else {
			r = a.Apply(ctx, org, env)
		}
		if r.Outcome == OutcomeError {
			failed = true
		}
		out.Results = append(out.Results, r)
		out.Counts[r.Outcome]++
	}
	// Leer, comparar y guardar el cursor bajo su candado: dos corridas
	// simultáneas del mismo flujo nunca lo hacen retroceder.
	err := withKey(ctx, &a.locks, a.cfg.Store, a.cfg.Atomic, cursorKey(org, b.Source, stream), func(ctx context.Context) error {
		prev, err := a.cfg.Store.GetCursor(ctx, org, b.Source, stream)
		if err != nil {
			return err
		}
		out.Cursor = prev
		if failed || b.Cursor == "" || CompareCursors(b.Cursor, prev) <= 0 {
			return nil
		}
		if err := a.cfg.Store.SetCursor(ctx, org, b.Source, stream, b.Cursor); err != nil {
			return err
		}
		out.Cursor, out.Advanced = b.Cursor, true
		return nil
	})
	return out, err
}

// Apply aplica un envelope. Nunca devuelve error: una falla de infraestructura
// es OutcomeError con el detalle.
func (a *Applier) Apply(ctx context.Context, org string, env Envelope) Result {
	if err := env.Validate(); err != nil {
		return a.review(ctx, org, env.Source, env, ReviewCodeInvalid, err.Error())
	}
	var res Result
	// Una fila a la vez (en el proceso y, con Locker, entre procesos): dos
	// corridas simultáneas no aplican versiones fuera de orden.
	err := withKey(ctx, &a.locks, a.cfg.Store, a.cfg.Atomic, rowKey(org, env.Source, env.Entity, env.ExternalID), func(ctx context.Context) error {
		var err error
		res, err = a.apply(ctx, org, env)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrRefConflict) {
			return a.review(ctx, org, env.Source, env, ReviewCodeRefConflict, err.Error())
		}
		if IsInvalid(err) {
			return a.review(ctx, org, env.Source, env, ReviewCodeInvalid, err.Error())
		}
		a.cfg.Logger.Error("aplicar envelope", "org", org, "entity", env.Entity, "external_id", env.ExternalID, "err", err)
		return Result{Entity: env.Entity, ExternalID: env.ExternalID, Outcome: OutcomeError, Detail: err.Error()}
	}
	switch res.Outcome {
	case OutcomeConflict:
		return a.review(ctx, org, env.Source, env, ReviewCodeOwnerConflict, res.Detail)
	case OutcomeReview:
		return a.review(ctx, org, env.Source, env, ReviewCodeDependencyMissing, res.Detail)
	}
	return res
}

func (a *Applier) apply(ctx context.Context, org string, env Envelope) (Result, error) {
	st, sys := a.cfg.Store, env.Source
	res := Result{Entity: env.Entity, ExternalID: env.ExternalID}
	hash := env.Hash()

	ref, found, err := st.GetRef(ctx, org, sys, env.Entity, env.ExternalID)
	if err != nil {
		return res, err
	}
	if found {
		res.LocalID = ref.LocalID
		order := CompareVersions(env.SourceVersion, ref.SourceVersion)
		if order == VersionStale {
			res.Outcome = OutcomeStale
			return res, nil
		}
		// Eco: la versión que vuelve es la que el propio puente produjo con un
		// comando (o el mismo dato). Solo se confirma el mapeo.
		if order == VersionSame || (hash == ref.LastHash && env.Op == OpUpsert && ref.State != RefDeleted) {
			if ref.State == RefPendingLegacy {
				ref.State, ref.SourceVersion, ref.LastHash = RefSynced, env.SourceVersion, hash
				if err := st.PutRef(ctx, ref); err != nil {
					return res, err
				}
				res.Outcome = OutcomeConfirmed
				return res, a.clearPending(ctx, org, env)
			}
			if order == VersionNewer {
				ref.SourceVersion = env.SourceVersion
				if err := st.PutRef(ctx, ref); err != nil {
					return res, err
				}
			}
			res.Outcome = OutcomeDuplicate
			return res, a.clearPending(ctx, org, env)
		}
	}

	branch, missingBranch, err := a.localBranch(ctx, org, env)
	if err != nil {
		return res, err
	}
	rules, err := st.Rules(ctx, org, sys)
	if err != nil {
		return res, err
	}
	phase, err := st.Phase(ctx, org, sys)
	if err != nil {
		return res, err
	}
	if !missingBranch {
		if owner := ResolveOwner(rules, env.Entity, branch, phase, a.cfg.DefaultOwner); owner != OwnerLegacy {
			res.Outcome = OutcomeConflict
			res.Detail = fmt.Sprintf("el dueño de %s en la sucursal %q (fase %q) es el host", env.Entity, branch, phase)
			return res, nil
		}
	}

	w := Write{Org: org, System: sys, Envelope: env, Created: !found}
	if found {
		w.LocalID = ref.LocalID
	} else {
		w.LocalID = a.cfg.Sink.DeriveID(org, env.Entity, env.ExternalID)
	}
	res.LocalID = w.LocalID
	facetRefs := make([]Ref, len(env.Facets))
	for i, f := range env.Facets {
		fr, ok, err := st.GetRef(ctx, org, sys, f.Entity, f.ExternalID)
		if err != nil {
			return res, err
		}
		if !ok {
			fr = Ref{Org: org, System: sys, Entity: f.Entity, ExternalID: f.ExternalID, LocalID: a.cfg.Sink.DeriveID(org, f.Entity, f.ExternalID), CreatedIn: OwnerLegacy}
		}
		facetRefs[i] = fr
		w.FacetIDs = append(w.FacetIDs, fr.LocalID)
	}

	if env.Op == OpDelete {
		if err := a.cfg.Sink.Delete(ctx, w); err != nil {
			return res, err
		}
		if err := a.putRefs(ctx, org, env, w, ref, found, facetRefs, hash, RefDeleted); err != nil {
			return res, err
		}
		res.Outcome = OutcomeDeleted
		return res, a.clearPending(ctx, org, env)
	}

	missing, err := a.missingDeps(ctx, org, env)
	if err != nil {
		return res, err
	}
	if missingBranch {
		missing = append(missing, Dependency{Entity: a.cfg.BranchEntity, ExternalID: env.BranchExternalID})
	}
	if len(missing) > 0 {
		return a.wait(ctx, org, env, missing, res)
	}

	if err := a.cfg.Sink.Upsert(ctx, w); err != nil {
		return res, err
	}
	if err := a.putRefs(ctx, org, env, w, ref, found, facetRefs, hash, RefSynced); err != nil {
		return res, err
	}
	res.Outcome = OutcomeApplied
	return res, a.clearPending(ctx, org, env)
}

func (a *Applier) putRefs(ctx context.Context, org string, env Envelope, w Write, ref Ref, found bool, facets []Ref, hash string, state RefState) error {
	now := a.cfg.Now().UTC()
	if !found {
		ref = Ref{Org: org, System: env.Source, Entity: env.Entity, ExternalID: env.ExternalID, LocalID: w.LocalID, CreatedIn: OwnerLegacy}
	}
	ref.Owner, ref.SourceVersion, ref.LastHash, ref.State, ref.UpdatedAt = OwnerLegacy, env.SourceVersion, hash, state, now
	if err := a.cfg.Store.PutRef(ctx, ref); err != nil {
		return err
	}
	for _, fr := range facets {
		fr.Owner, fr.SourceVersion, fr.LastHash, fr.State, fr.UpdatedAt = OwnerLegacy, env.SourceVersion, hash, state, now
		if err := a.cfg.Store.PutRef(ctx, fr); err != nil {
			return err
		}
	}
	return nil
}

// localBranch traduce la sucursal del envelope al id local. missing=true si
// hay sucursal y BranchEntity pero todavía no está mapeada.
func (a *Applier) localBranch(ctx context.Context, org string, env Envelope) (string, bool, error) {
	ext := strings.TrimSpace(env.BranchExternalID)
	if ext == "" || a.cfg.BranchEntity == "" {
		return ext, false, nil
	}
	r, ok, err := a.cfg.Store.GetRef(ctx, org, env.Source, a.cfg.BranchEntity, ext)
	if err != nil {
		return "", false, err
	}
	if ok {
		return r.LocalID, false, nil
	}
	id := a.cfg.Sink.DeriveID(org, a.cfg.BranchEntity, ext)
	exists, err := a.cfg.Sink.Exists(ctx, org, a.cfg.BranchEntity, id)
	if err != nil {
		return "", false, err
	}
	if exists {
		return id, false, nil
	}
	return "", true, nil
}

func (a *Applier) missingDeps(ctx context.Context, org string, env Envelope) ([]Dependency, error) {
	var missing []Dependency
	for _, d := range env.Requires {
		r, ok, err := a.cfg.Store.GetRef(ctx, org, env.Source, d.Entity, d.ExternalID)
		if err != nil {
			return nil, err
		}
		if ok && r.State != RefDeleted {
			continue
		}
		id := a.cfg.Sink.DeriveID(org, d.Entity, d.ExternalID)
		if ok {
			id = r.LocalID
		}
		exists, err := a.cfg.Sink.Exists(ctx, org, d.Entity, id)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, d)
		}
	}
	return missing, nil
}

func (a *Applier) wait(ctx context.Context, org string, env Envelope, missing []Dependency, res Result) (Result, error) {
	now := a.cfg.Now().UTC()
	p, ok, err := a.cfg.Store.GetPending(ctx, org, env.Source, env.Entity, env.ExternalID)
	if err != nil {
		return res, err
	}
	if !ok {
		p = PendingEnvelope{Org: org, System: env.Source, FirstSeen: now}
	}
	if now.Sub(p.FirstSeen) >= a.cfg.MaxWait {
		if err := a.cfg.Store.DeletePending(ctx, org, env.Source, env.Entity, env.ExternalID); err != nil {
			return res, err
		}
		res.Outcome = OutcomeReview
		res.Detail = "dependencias faltantes tras " + a.cfg.MaxWait.String() + ": " + depList(missing)
		return res, nil
	}
	p.Envelope, p.Missing = env, missing
	p.Attempts++
	p.NextAttempt = now.Add(a.cfg.Backoff(p.Attempts))
	if err := a.cfg.Store.PutPending(ctx, p); err != nil {
		return res, err
	}
	res.Outcome = OutcomeWaiting
	res.Detail = "faltan: " + depList(missing)
	return res, nil
}

func (a *Applier) clearPending(ctx context.Context, org string, env Envelope) error {
	return a.cfg.Store.DeletePending(ctx, org, env.Source, env.Entity, env.ExternalID)
}

// RetryWaiting reintenta los envelopes en espera que ya vencieron. Devuelve
// los resultados; los que vencen MaxWait van a revisión.
func (a *Applier) RetryWaiting(ctx context.Context, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 200
	}
	due, err := a.cfg.Store.DuePending(ctx, a.cfg.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(due))
	for _, p := range due {
		out = append(out, a.Apply(ctx, p.Org, p.Envelope))
	}
	return out, nil
}

func (a *Applier) review(ctx context.Context, org, system string, env Envelope, code, detail string) Result {
	res := Result{Entity: env.Entity, ExternalID: env.ExternalID, Outcome: OutcomeReview, Detail: detail}
	if code == ReviewCodeOwnerConflict {
		res.Outcome = OutcomeConflict
	}
	if a.cfg.Review == nil {
		a.cfg.Logger.Warn("envelope a revisión sin ReviewStore", "org", org, "entity", env.Entity, "external_id", env.ExternalID, "code", code)
		return res
	}
	var rec map[string]any
	b, _ := json.Marshal(env)
	_ = json.Unmarshal(b, &rec)
	now := a.cfg.Now().UTC()
	entity := env.Entity
	if entity == "" {
		entity = "unknown"
	}
	e := dataload.ReviewEntry{
		Org: org, Entity: entity, Key: system + "|" + env.ExternalID,
		Profile: ReviewProfile, ProfileVersion: system, RunID: system + ":" + env.SourceVersion,
		Hash: env.Hash(), Record: rec,
		Items:   []dataload.ReviewItem{{Code: code, Severity: "error", Detail: detail}},
		Outcome: dataload.ActionReview, State: dataload.ReviewPending,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := a.cfg.Review.Put(ctx, e); err != nil {
		a.cfg.Logger.Error("guardar revisión", "org", org, "entity", env.Entity, "external_id", env.ExternalID, "err", err)
		res.Outcome, res.Detail = OutcomeError, err.Error()
	}
	return res
}

func depList(ds []Dependency) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.Entity + ":" + d.ExternalID
	}
	return strings.Join(parts, ", ")
}
