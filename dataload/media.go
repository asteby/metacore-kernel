package dataload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/importer"
)

// Esta parte implementa el respaldo de medios. Regla: el original descargado se
// guarda tal cual, inmutable y direccionado por su sha256, en el almacén propio
// (HashedMediaStore); la tienda no depende del sitio origen. Los derivados
// (WebP, tamaños) se generan aparte y nunca reemplazan al original.

// BackupStore es el almacén de originales: HashedMediaStore del importador más
// lectura, necesaria para verificar lo guardado.
type BackupStore interface {
	importer.HashedMediaStore
	// Open lee un objeto ya guardado por su nombre.
	Open(ctx context.Context, storedName string) (io.ReadCloser, error)
}

// EntryStatus es el resultado de la última verificación de un respaldo.
type EntryStatus string

const (
	EntryOK      EntryStatus = "ok"
	EntryMissing EntryStatus = "missing"
	EntryCorrupt EntryStatus = "corrupt"
)

// ManifestEntry documenta un original respaldado: de dónde vino, qué es y
// cuándo se verificó por última vez.
type ManifestEntry struct {
	Org         string      `json:"org"`
	SourceURL   string      `json:"sourceUrl"`
	SHA256      string      `json:"sha256"`
	Bytes       int64       `json:"bytes"`
	ContentType string      `json:"contentType"`
	StoredName  string      `json:"storedName"`
	FetchedAt   time.Time   `json:"fetchedAt"`
	VerifiedAt  time.Time   `json:"verifiedAt"`
	Status      EntryStatus `json:"status"`
	// ETag y LastModified son los validadores que mandó el origen al bajar este
	// original; con ellos se revalida sin volver a bajar el archivo.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// ValidatedAt es la última vez que se confirmó contra el origen que este
	// original sigue vigente (al bajarlo o al revalidarlo).
	ValidatedAt time.Time `json:"validatedAt,omitempty"`
	// Revisions son los originales anteriores de la misma URL, del más viejo al
	// más nuevo: cuando el origen cambia el archivo se guarda uno nuevo con otro
	// sha256 y el anterior queda intacto en el almacén.
	Revisions []ManifestRevision `json:"revisions,omitempty"`
}

// ManifestRevision documenta un original que dejó de ser el vigente de su URL.
type ManifestRevision struct {
	SHA256       string    `json:"sha256"`
	Bytes        int64     `json:"bytes"`
	ContentType  string    `json:"contentType"`
	StoredName   string    `json:"storedName"`
	FetchedAt    time.Time `json:"fetchedAt"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"lastModified,omitempty"`
}

// Manifest es el registro persistente de respaldos; lo implementa ops/addons.
type Manifest interface {
	// Put inserta o reemplaza la entrada de (Org, SourceURL).
	Put(ctx context.Context, e ManifestEntry) error
	// ByURL devuelve la entrada de la URL en la organización.
	ByURL(ctx context.Context, org, url string) (ManifestEntry, bool, error)
	// List devuelve todas las entradas de la organización, ordenadas por URL.
	List(ctx context.Context, org string) ([]ManifestEntry, error)
}

// Errores de respaldo.
var (
	// ErrBackupCorrupt: lo leído del almacén no coincide con el sha256 descargado.
	ErrBackupCorrupt = errors.New("dataload: el respaldo no coincide con el sha256 descargado")
	// ErrBackupMissing: el almacén no tiene el objeto que el manifiesto promete.
	ErrBackupMissing = errors.New("dataload: el respaldo no está en el almacén")
)

// RevalidateMode dice cómo comprobar que un original ya respaldado sigue vigente.
type RevalidateMode string

const (
	// RevalidateNone: lo respaldado se da por bueno; no se toca la red.
	RevalidateNone RevalidateMode = ""
	// RevalidateConditional pide al origen solo si cambió (If-None-Match /
	// If-Modified-Since con los validadores guardados). 304 = nada que bajar.
	// Sin validadores guardados degrada a refetch.
	RevalidateConditional RevalidateMode = "conditional"
	// RevalidateRefetch baja el archivo de nuevo sin condiciones. Si el contenido
	// cambió se guarda un original nuevo con otro sha256.
	RevalidateRefetch RevalidateMode = "refetch"
)

// RevalidatePolicy es la política opcional de revalidación de un Backuper: pasado
// MaxAge desde la última confirmación, Backup revalida según Mode. El valor cero
// no revalida nunca (comportamiento de siempre).
type RevalidatePolicy struct {
	Mode   RevalidateMode
	MaxAge time.Duration
}

// BackupOutcome es lo que pasó con un original al pedir su respaldo.
type BackupOutcome string

const (
	OutcomeNew         BackupOutcome = "new"          // no existía: se bajó y respaldó
	OutcomeFresh       BackupOutcome = "fresh"        // ya respaldado y vigente por política: no se tocó la red
	OutcomeNotModified BackupOutcome = "not_modified" // el origen contestó 304
	OutcomeUnchanged   BackupOutcome = "unchanged"    // se bajó de nuevo y el contenido es el mismo (mismo sha256)
	OutcomeReplaced    BackupOutcome = "replaced"     // el contenido cambió: nuevo original con otro sha256
	// OutcomeStale: no se pudo revalidar (origen caído, etc.); sigue vigente el original anterior.
	OutcomeStale BackupOutcome = "stale"
)

// BackupResult acompaña a la entrada con lo ocurrido.
type BackupResult struct {
	Outcome BackupOutcome
	// Err solo viene con OutcomeStale: por qué no se pudo revalidar.
	Err error
}

// Backuper descarga, respalda y verifica originales.
type Backuper struct {
	Store    BackupStore
	Manifest Manifest
	// Fetch son las dependencias de descarga (cliente HTTP, MediaPolicy, Sleep).
	// Store y Context se ignoran. Con HTTPClient nil se usa el cliente con guarda
	// de netguard. MaxAttempts por omisión es 2: la cola reintenta más tarde.
	Fetch importer.TransformDeps
	// Revalidate es la política opcional de revalidación; vacía = nunca.
	Revalidate RevalidatePolicy
	// Now es el reloj; default time.Now.
	Now func() time.Time
}

func (b *Backuper) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

// Backup deja un original respaldado y verificado y devuelve su entrada. Si el
// manifiesto ya tiene la URL con estado ok no toca la red (salvo que la política
// Revalidate lo pida): repetir una corrida no descarga de nuevo. Un objeto que ya
// existe en el almacén con ese sha256 no se reescribe.
func (b *Backuper) Backup(ctx context.Context, org, rawURL string) (ManifestEntry, error) {
	e, _, err := b.Ensure(ctx, org, rawURL, RevalidateNone)
	return e, err
}

// Refetch baja de nuevo el original de la URL sin condiciones (decisión explícita
// de quien opera). Si el contenido cambió guarda un original nuevo con otro
// sha256; el anterior queda en el almacén y en Revisions.
func (b *Backuper) Refetch(ctx context.Context, org, rawURL string) (ManifestEntry, BackupResult, error) {
	return b.Ensure(ctx, org, rawURL, RevalidateRefetch)
}

// Ensure es Backup con control de la revalidación. mode vacío aplica la política
// del Backuper; un modo explícito la ignora y revalida ya. Un fallo al revalidar
// NO es un error: el original anterior sigue siendo bueno (OutcomeStale).
func (b *Backuper) Ensure(ctx context.Context, org, rawURL string, mode RevalidateMode) (ManifestEntry, BackupResult, error) {
	prev, ok, err := b.Manifest.ByURL(ctx, org, rawURL)
	if err != nil {
		return ManifestEntry{}, BackupResult{}, err
	}
	if !ok || prev.Status != EntryOK {
		e, err := b.fetchAndStore(ctx, org, rawURL, nil, importer.FetchOptions{})
		return e, BackupResult{Outcome: OutcomeNew}, err
	}
	if mode == RevalidateNone {
		pol := b.Revalidate
		since := prev.ValidatedAt
		if since.IsZero() {
			since = prev.FetchedAt
		}
		if pol.Mode == RevalidateNone || b.now().Sub(since) < pol.MaxAge {
			return prev, BackupResult{Outcome: OutcomeFresh}, nil
		}
		mode = pol.Mode
	}
	opt := importer.FetchOptions{}
	if mode == RevalidateConditional {
		switch {
		case prev.ETag != "":
			opt.IfNoneMatch = prev.ETag
		case prev.LastModified != "":
			opt.IfModifiedSince = prev.LastModified
		}
	}
	e, res, err := b.revalidate(ctx, org, rawURL, prev, opt)
	if err != nil {
		if ctx.Err() != nil {
			return ManifestEntry{}, BackupResult{}, ctx.Err()
		}
		return prev, BackupResult{Outcome: OutcomeStale, Err: err}, nil
	}
	return e, res, nil
}

func (b *Backuper) revalidate(ctx context.Context, org, rawURL string, prev ManifestEntry, opt importer.FetchOptions) (ManifestEntry, BackupResult, error) {
	f, err := b.fetch(ctx, rawURL, opt)
	if err != nil {
		return ManifestEntry{}, BackupResult{}, err
	}
	if f.NotModified {
		prev.ValidatedAt = b.now()
		if err := b.Manifest.Put(ctx, prev); err != nil {
			return ManifestEntry{}, BackupResult{}, fmt.Errorf("dataload: escribir manifiesto: %w", err)
		}
		return prev, BackupResult{Outcome: OutcomeNotModified}, nil
	}
	sum := sha256.Sum256(f.Data)
	if hex.EncodeToString(sum[:]) == prev.SHA256 {
		prev.ETag, prev.LastModified, prev.ValidatedAt = f.ETag, f.LastModified, b.now()
		if err := b.Manifest.Put(ctx, prev); err != nil {
			return ManifestEntry{}, BackupResult{}, fmt.Errorf("dataload: escribir manifiesto: %w", err)
		}
		return prev, BackupResult{Outcome: OutcomeUnchanged}, nil
	}
	// Contenido nuevo: otro original, otro sha256. El anterior no se toca.
	e, err := b.store(ctx, org, rawURL, f, &prev)
	if err != nil {
		return ManifestEntry{}, BackupResult{}, err
	}
	return e, BackupResult{Outcome: OutcomeReplaced}, nil
}

func (b *Backuper) fetch(ctx context.Context, rawURL string, opt importer.FetchOptions) (*importer.FetchedMedia, error) {
	deps := b.Fetch
	deps.Store, deps.Context = nil, ctx
	if deps.Policy.MaxAttempts == 0 {
		deps.Policy.MaxAttempts = 2
	}
	return importer.FetchMediaWith(&deps, rawURL, opt)
}

func (b *Backuper) fetchAndStore(ctx context.Context, org, rawURL string, prev *ManifestEntry, opt importer.FetchOptions) (ManifestEntry, error) {
	f, err := b.fetch(ctx, rawURL, opt)
	if err != nil {
		return ManifestEntry{}, err
	}
	return b.store(ctx, org, rawURL, f, prev)
}

// store respalda los bytes bajados (sin reescribir un objeto que ya existe con
// ese sha256), verifica y deja la entrada en el manifiesto. Si prev viene, pasa
// a Revisions.
func (b *Backuper) store(ctx context.Context, org, rawURL string, f *importer.FetchedMedia, prev *ManifestEntry) (ManifestEntry, error) {
	sum := sha256.Sum256(f.Data)
	hexsum := hex.EncodeToString(sum[:])

	name, found, err := b.Store.Lookup(ctx, hexsum)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("dataload: consultar almacén: %w", err)
	}
	if !found {
		if name, err = b.Store.PutHashed(ctx, hexsum, f.SuggestedName, bytes.NewReader(f.Data), f.Type); err != nil {
			return ManifestEntry{}, fmt.Errorf("dataload: guardar original: %w", err)
		}
	}
	entry := ManifestEntry{
		Org: org, SourceURL: rawURL, SHA256: hexsum, Bytes: int64(len(f.Data)), ContentType: f.Type,
		StoredName: name, FetchedAt: b.now(), ValidatedAt: b.now(), Status: EntryOK, ETag: f.ETag, LastModified: f.LastModified,
	}
	if prev != nil {
		entry.Revisions = append(append([]ManifestRevision(nil), prev.Revisions...), ManifestRevision{
			SHA256: prev.SHA256, Bytes: prev.Bytes, ContentType: prev.ContentType, StoredName: prev.StoredName,
			FetchedAt: prev.FetchedAt, ETag: prev.ETag, LastModified: prev.LastModified,
		})
	}
	if err := b.verify(ctx, &entry); err != nil {
		if prev == nil && (entry.Status == EntryCorrupt || entry.Status == EntryMissing) {
			_ = b.Manifest.Put(ctx, entry) // queda constancia para la auditoría
		}
		return ManifestEntry{}, err
	}
	if err := b.Manifest.Put(ctx, entry); err != nil {
		return ManifestEntry{}, fmt.Errorf("dataload: escribir manifiesto: %w", err)
	}
	return entry, nil
}

// verify relee el objeto del almacén y compara tamaño y sha256 con la entrada.
func (b *Backuper) verify(ctx context.Context, e *ManifestEntry) error {
	rc, err := b.Store.Open(ctx, e.StoredName)
	if err != nil {
		e.Status = EntryMissing
		return fmt.Errorf("%w: %s: %v", ErrBackupMissing, e.StoredName, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return fmt.Errorf("dataload: leer respaldo %s: %w", e.StoredName, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 || n != e.Bytes {
		e.Status = EntryCorrupt
		return fmt.Errorf("%w: %s", ErrBackupCorrupt, e.StoredName)
	}
	e.Status, e.VerifiedAt = EntryOK, b.now()
	return nil
}

// VerifyReport es el resultado de auditar todo el manifiesto de una organización.
type VerifyReport struct {
	Checked int             `json:"checked"`
	OK      int             `json:"ok"`
	Missing []ManifestEntry `json:"missing,omitempty"`
	Corrupt []ManifestEntry `json:"corrupt,omitempty"`
}

// Healthy es true cuando todo lo del manifiesto está íntegro.
func (r VerifyReport) Healthy() bool { return len(r.Missing) == 0 && len(r.Corrupt) == 0 }

// VerifyAll relee cada original del manifiesto y actualiza su estado.
func (b *Backuper) VerifyAll(ctx context.Context, org string) (VerifyReport, error) {
	entries, err := b.Manifest.List(ctx, org)
	if err != nil {
		return VerifyReport{}, err
	}
	var rep VerifyReport
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Checked++
		verr := b.verify(ctx, &e)
		switch {
		case verr == nil:
			rep.OK++
		case errors.Is(verr, ErrBackupMissing):
			rep.Missing = append(rep.Missing, e)
		case errors.Is(verr, ErrBackupCorrupt):
			rep.Corrupt = append(rep.Corrupt, e)
		default:
			return rep, verr
		}
		if err := b.Manifest.Put(ctx, e); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// MediaState es el estado de un trabajo de medios.
type MediaState string

const (
	MediaPending MediaState = "pending" // esperando turno o reintento
	MediaRunning MediaState = "running"
	MediaDone    MediaState = "done"
	MediaDead    MediaState = "dead" // falló de forma definitiva o agotó los intentos
)

// MediaJob es la descarga y respaldo de una URL para un campo de un registro.
type MediaJob struct {
	ID        string     `json:"id"` // determinista: (org, entidad, key, campo, url)
	Org       string     `json:"org"`
	Entity    string     `json:"entity"`
	RunID     string     `json:"runId,omitempty"`
	Key       string     `json:"key"`
	Field     string     `json:"field"`
	Index     int        `json:"index"` // posición en la lista del campo
	URL       string     `json:"url"`
	State     MediaState `json:"state"`
	Attempts  int        `json:"attempts"`
	LastError string     `json:"lastError,omitempty"`
	// NextAttempt es cuándo puede volver a intentarse (pending) o cuándo vence
	// el arrendamiento (running).
	NextAttempt time.Time `json:"nextAttempt"`
	SHA256      string    `json:"sha256,omitempty"`
	StoredName  string    `json:"storedName,omitempty"`
	// Revalidate, si no está vacío, pide revalidar el original ya respaldado en
	// vez de solo asegurarlo (ver MediaQueue.Revalidate). Se limpia al terminar.
	Revalidate RevalidateMode `json:"revalidate,omitempty"`
	// Outcome es lo que pasó en el último intento exitoso (nuevo, vigente, 304, reemplazado...).
	Outcome BackupOutcome `json:"outcome,omitempty"`
	// DeriveError guarda el fallo del generador de derivados; no invalida el original.
	DeriveError string    `json:"deriveError,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// MediaJobFilter acota List.
type MediaJobFilter struct {
	RunID  string
	State  MediaState
	Entity string
	Key    string
	Field  string
}

// MediaJobStore es la cola persistente; la implementa ops/addons (p. ej. sobre
// el outbox). El progreso vive aquí, no en memoria: la cola se reanuda tras un
// reinicio o una caída de red.
type MediaJobStore interface {
	// Enqueue inserta los trabajos que no existan (por ID) y devuelve cuántos
	// eran nuevos. Nunca reinicia uno existente.
	Enqueue(ctx context.Context, jobs []MediaJob) (added int, err error)
	// Claim toma hasta limit trabajos pending cuya NextAttempt ya pasó, o running
	// con arrendamiento vencido, los marca running con NextAttempt = now+lease y
	// los devuelve. Debe ser seguro con varios workers.
	Claim(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]MediaJob, error)
	Update(ctx context.Context, job MediaJob) error
	List(ctx context.Context, org string, f MediaJobFilter) ([]MediaJob, error)
	// RequeueDead devuelve a pending (intentos en 0) los trabajos dead de la
	// corrida (o de toda la organización si runID está vacío): "Reintentar medios".
	RequeueDead(ctx context.Context, org, runID string, now time.Time) (int, error)
}

// MediaRef describe un original respaldado ligado a un campo.
type MediaRef struct {
	Index       int    `json:"index"`
	SourceURL   string `json:"sourceUrl"`
	SHA256      string `json:"sha256"`
	StoredName  string `json:"storedName"`
	ContentType string `json:"contentType"`
	Bytes       int64  `json:"bytes"`
}

// MediaSink liga los originales respaldados al registro. Lo implementa
// ops/addons (galería del producto, asset, etc.).
type MediaSink interface {
	// Attach recibe TODAS las referencias ya respaldadas del campo, en orden, y
	// reemplaza las anteriores (idempotente). Se llama cuando ningún trabajo del
	// campo sigue pendiente; un medio fallido simplemente no aparece (el
	// producto queda con su placeholder). Si luego se reintenta y termina, se
	// vuelve a llamar con el conjunto completo.
	Attach(ctx context.Context, org, entity, key, field string, refs []MediaRef) error
}

// Deriver genera derivados (WebP, tamaños) a partir de un original ya
// respaldado. Es opcional y vive en ops/addons: el kernel no procesa imágenes.
// Debe escribir en otro lugar; nunca modifica el original.
type Deriver interface {
	Derive(ctx context.Context, e ManifestEntry) error
}

// MediaQueue procesa la cola: descarga con netguard/MediaPolicy, respalda cada
// original por sha256 con manifiesto, reintenta con espera creciente y liga el
// resultado al registro.
type MediaQueue struct {
	Jobs   MediaJobStore
	Backup *Backuper
	Sink   MediaSink // opcional
	// Deriver opcional; un fallo se anota en el trabajo sin invalidar el original.
	Deriver Deriver
	// MaxAttempts por trabajo; default 5.
	MaxAttempts int
	// RetryDelays es la espera tras el intento n (se repite el último valor).
	// Default: 2s, 8s, 30s, 2min, 10min. Un 429/5xx con Retry-After no lo acorta
	// más allá de lo que el cliente ya espera dentro del intento.
	RetryDelays []time.Duration
	// Limits da los topes de una organización (concurrencia y reintentos); nil o
	// el valor cero = sin tope propio. Ver OrgMediaLimits.
	Limits func(org string) OrgMediaLimits
	// Workers en paralelo por pasada; default 4.
	Workers int
	// Lease es cuánto se reserva un trabajo; default 5min.
	Lease time.Duration
	Now   func() time.Time

	orgMu     sync.Mutex
	orgActive map[string]int
}

// OrgMediaLimits son los topes de una organización en la cola de medios, para
// que una carga enorme de un tenant no se coma a los demás.
type OrgMediaLimits struct {
	// MaxConcurrent es cuántos trabajos de la organización se procesan a la vez
	// en este proceso (0 = solo vale el tope Workers de la cola). Con varios
	// procesos el tope es por proceso.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// MaxAttempts es el tope de intentos por trabajo de la organización (0 = el de la cola).
	MaxAttempts int `json:"maxAttempts,omitempty"`
}

// StaticOrgLimits arma un Limits con un valor por defecto y excepciones por organización.
func StaticOrgLimits(def OrgMediaLimits, per map[string]OrgMediaLimits) func(string) OrgMediaLimits {
	return func(org string) OrgMediaLimits {
		if l, ok := per[org]; ok {
			return l
		}
		return def
	}
}

func (q *MediaQueue) limits(org string) OrgMediaLimits {
	if q.Limits == nil {
		return OrgMediaLimits{}
	}
	return q.Limits(org)
}

func (q *MediaQueue) maxAttemptsFor(org string) int {
	if n := q.limits(org).MaxAttempts; n > 0 {
		return n
	}
	return q.maxAttempts()
}

// acquireOrg reserva un lugar de la organización; false si ya está en su tope.
func (q *MediaQueue) acquireOrg(org string) bool {
	q.orgMu.Lock()
	defer q.orgMu.Unlock()
	if q.orgActive == nil {
		q.orgActive = map[string]int{}
	}
	if max := q.limits(org).MaxConcurrent; max > 0 && q.orgActive[org] >= max {
		return false
	}
	q.orgActive[org]++
	return true
}

func (q *MediaQueue) releaseOrg(org string) {
	q.orgMu.Lock()
	defer q.orgMu.Unlock()
	if q.orgActive[org] > 0 {
		q.orgActive[org]--
	}
}

// ActiveForOrg dice cuántos trabajos de la organización se están procesando ahora en este proceso.
func (q *MediaQueue) ActiveForOrg(org string) int {
	q.orgMu.Lock()
	defer q.orgMu.Unlock()
	return q.orgActive[org]
}

var defaultRetryDelays = []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

func (q *MediaQueue) now() time.Time {
	if q.Now != nil {
		return q.Now().UTC()
	}
	return time.Now().UTC()
}

func (q *MediaQueue) maxAttempts() int {
	if q.MaxAttempts > 0 {
		return q.MaxAttempts
	}
	return 5
}

func (q *MediaQueue) workers() int {
	if q.Workers > 0 {
		return q.Workers
	}
	return 4
}

func (q *MediaQueue) lease() time.Duration {
	if q.Lease > 0 {
		return q.Lease
	}
	return 5 * time.Minute
}

func (q *MediaQueue) delay(attempt int) time.Duration {
	d := q.RetryDelays
	if len(d) == 0 {
		d = defaultRetryDelays
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(d) {
		attempt = len(d)
	}
	return d[attempt-1]
}

// jobID es determinista: la misma URL del mismo campo del mismo registro es un solo trabajo.
func jobID(org, entity, key, field, url string) string {
	sum := sha256.Sum256([]byte(org + "\x00" + entity + "\x00" + key + "\x00" + field + "\x00" + url))
	return hex.EncodeToString(sum[:16])
}

func (q *MediaQueue) jobsFor(org, entity, runID, key string, media map[string][]string) []MediaJob {
	fields := make([]string, 0, len(media))
	for f := range media {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	var out []MediaJob
	for _, f := range fields {
		for i, u := range media[f] {
			out = append(out, MediaJob{
				ID: jobID(org, entity, key, f, u), Org: org, Entity: entity, RunID: runID, Key: key,
				Field: f, Index: i, URL: u, State: MediaPending, NextAttempt: q.now(), UpdatedAt: q.now(),
			})
		}
	}
	return out
}

// Enqueue agrega trabajos (idempotente por ID).
func (q *MediaQueue) Enqueue(ctx context.Context, jobs []MediaJob) (int, error) {
	return q.Jobs.Enqueue(ctx, jobs)
}

// RetryFailed devuelve a la cola los trabajos definitivamente fallidos.
func (q *MediaQueue) RetryFailed(ctx context.Context, org, runID string) (int, error) {
	return q.Jobs.RequeueDead(ctx, org, runID, q.now())
}

// ProcessOnce reclama y procesa hasta limit trabajos vencidos con Workers en
// paralelo, respetando el tope de concurrencia de cada organización (Limits). Los
// trabajos reclamados se reparten por turnos entre organizaciones, así una con
// miles de medios no hace esperar a las demás dentro de la misma pasada; los que
// no caben por su tope esperan su turno sin gastar intentos. Devuelve cuántos
// procesó. Un worker del outbox lo llama en bucle.
func (q *MediaQueue) ProcessOnce(ctx context.Context, limit int) (int, error) {
	jobs, err := q.Jobs.Claim(ctx, q.now(), limit, q.lease())
	if err != nil {
		return 0, err
	}
	pending := roundRobinByOrg(jobs)
	slots := make(chan struct{}, q.workers())
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	running := 0
	processed := 0

	for len(pending) > 0 {
		if ctx.Err() != nil {
			// Apagado: lo reclamado y no iniciado vuelve a la cola sin gastar un intento.
			for _, j := range pending {
				j.State, j.NextAttempt, j.UpdatedAt = MediaPending, q.now(), q.now()
				_ = q.Jobs.Update(context.WithoutCancel(ctx), j)
			}
			break
		}
		var rest []MediaJob
		launched := false
		for _, j := range pending {
			select {
			case slots <- struct{}{}:
			default:
				rest = append(rest, j)
				continue
			}
			if !q.acquireOrg(j.Org) {
				<-slots
				rest = append(rest, j)
				continue
			}
			launched = true
			mu.Lock()
			running++
			processed++
			mu.Unlock()
			wg.Add(1)
			go func(j MediaJob) {
				defer wg.Done()
				err := q.process(ctx, j)
				q.releaseOrg(j.Org)
				<-slots
				mu.Lock()
				running--
				if err != nil && firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				select {
				case wake <- struct{}{}:
				default:
				}
			}(j)
		}
		pending = rest
		if len(pending) > 0 && !launched {
			// Todo lo pendiente está frenado por topes: se espera a que algo termine
			// (en esta pasada o en otra que comparte los topes de la organización).
			select {
			case <-wake:
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}
	wg.Wait()
	return processed, firstErr
}

// roundRobinByOrg reparte los trabajos por turnos entre organizaciones sin
// alterar el orden relativo dentro de cada una.
func roundRobinByOrg(jobs []MediaJob) []MediaJob {
	byOrg := map[string][]MediaJob{}
	var order []string
	for _, j := range jobs {
		if _, ok := byOrg[j.Org]; !ok {
			order = append(order, j.Org)
		}
		byOrg[j.Org] = append(byOrg[j.Org], j)
	}
	out := make([]MediaJob, 0, len(jobs))
	for len(out) < len(jobs) {
		for _, o := range order {
			if q := byOrg[o]; len(q) > 0 {
				out = append(out, q[0])
				byOrg[o] = q[1:]
			}
		}
	}
	return out
}

// Revalidate vuelve a poner en cola, para revalidar su original, los trabajos ya
// terminados que cumplan el filtro (p. ej. los de una organización, o los de un
// campo). mode: RevalidateConditional (If-None-Match / If-Modified-Since) o
// RevalidateRefetch (bajar de nuevo; si cambió, nuevo original con otro sha256).
// Un fallo al revalidar no invalida el original anterior.
func (q *MediaQueue) Revalidate(ctx context.Context, org string, f MediaJobFilter, mode RevalidateMode) (int, error) {
	if mode != RevalidateConditional && mode != RevalidateRefetch {
		return 0, fmt.Errorf("dataload: modo de revalidación desconocido %q", mode)
	}
	f.State = MediaDone
	jobs, err := q.Jobs.List(ctx, org, f)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		j.State, j.Revalidate, j.Attempts, j.NextAttempt, j.UpdatedAt = MediaPending, mode, 0, q.now(), q.now()
		if err := q.Jobs.Update(ctx, j); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Run procesa la cola en bucle hasta que ctx termine, esperando poll cuando no
// hay nada vencido.
func (q *MediaQueue) Run(ctx context.Context, poll time.Duration) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		n, err := q.ProcessOnce(ctx, 50)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || n == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
			}
		}
	}
}

func (q *MediaQueue) process(ctx context.Context, j MediaJob) error {
	j.Attempts++
	entry, res, err := q.Backup.Ensure(ctx, j.Org, j.URL, j.Revalidate)
	j.UpdatedAt = q.now()
	switch {
	case err == nil:
		j.State, j.SHA256, j.StoredName, j.LastError = MediaDone, entry.SHA256, entry.StoredName, ""
		j.Revalidate, j.Outcome = RevalidateNone, res.Outcome
		if res.Err != nil {
			j.LastError = "revalidación: " + res.Err.Error() // el original anterior sigue vigente
		}
		if q.Deriver != nil {
			if derr := q.Deriver.Derive(ctx, entry); derr != nil {
				j.DeriveError = derr.Error()
			}
		}
	case ctx.Err() != nil:
		// Apagado o cancelación: no cuenta como intento fallido; se libera para
		// que otro worker lo tome sin esperar el vencimiento del arrendamiento.
		j.Attempts--
		j.State, j.NextAttempt = MediaPending, q.now()
		_ = q.Jobs.Update(context.WithoutCancel(ctx), j)
		return ctx.Err()
	case !importer.IsRetryable(ctx, err) || errors.Is(err, ErrBackupCorrupt) || j.Attempts >= q.maxAttemptsFor(j.Org):
		j.State, j.LastError = MediaDead, err.Error()
	default:
		j.State, j.LastError, j.NextAttempt = MediaPending, err.Error(), q.now().Add(q.delay(j.Attempts))
	}
	if uerr := q.Jobs.Update(ctx, j); uerr != nil {
		return uerr
	}
	if j.State == MediaDone || j.State == MediaDead {
		return q.attachIfSettled(ctx, j)
	}
	return nil
}

// attachIfSettled liga al registro el conjunto de originales del campo cuando
// ya no queda ningún trabajo pendiente o en curso para él.
func (q *MediaQueue) attachIfSettled(ctx context.Context, j MediaJob) error {
	if q.Sink == nil {
		return nil
	}
	group, err := q.Jobs.List(ctx, j.Org, MediaJobFilter{Entity: j.Entity, Key: j.Key, Field: j.Field})
	if err != nil {
		return err
	}
	sort.Slice(group, func(a, b int) bool { return group[a].Index < group[b].Index })
	var refs []MediaRef
	for _, g := range group {
		switch g.State {
		case MediaPending, MediaRunning:
			return nil
		case MediaDone:
			e, ok, err := q.Backup.Manifest.ByURL(ctx, g.Org, g.URL)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			refs = append(refs, MediaRef{Index: len(refs), SourceURL: g.URL, SHA256: e.SHA256, StoredName: e.StoredName, ContentType: e.ContentType, Bytes: e.Bytes})
		}
	}
	return q.Sink.Attach(ctx, j.Org, j.Entity, j.Key, j.Field, refs)
}

// MediaStats resume la cola de una corrida (o de toda la organización).
type MediaStats struct {
	Pending, Running, Done, Dead int
}

// Stats cuenta los trabajos por estado.
func (q *MediaQueue) Stats(ctx context.Context, org, runID string) (MediaStats, error) {
	jobs, err := q.Jobs.List(ctx, org, MediaJobFilter{RunID: runID})
	if err != nil {
		return MediaStats{}, err
	}
	var s MediaStats
	for _, j := range jobs {
		switch j.State {
		case MediaPending:
			s.Pending++
		case MediaRunning:
			s.Running++
		case MediaDone:
			s.Done++
		case MediaDead:
			s.Dead++
		}
	}
	return s, nil
}

// ExportManifest serializa el manifiesto de una organización como JSON (url
// origen, sha256, bytes, tipo, fecha, estado) para guardarlo junto al respaldo.
func ExportManifest(ctx context.Context, m Manifest, org string) ([]byte, error) {
	entries, err := m.List(ctx, org)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []ManifestEntry{}
	}
	return json.MarshalIndent(entries, "", "  ")
}
