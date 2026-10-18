package dataload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Conjunto vigente de medios. La cola guarda trabajos de descarga por URL y
// nunca los borra (son el historial de respaldos). Lo que se liga al registro no
// sale de ese historial sino del CONJUNTO VIGENTE de cada campo: las URLs, en
// orden, que trajo la última escritura de esa identidad (corrida o revisión
// aplicada). Así una imagen que se quita en una carga posterior deja de ligarse
// y un cambio de orden se respeta aunque el trabajo de la URL ya exista.
//
// El conjunto también recuerda qué se ligó por última vez (Attached). Sync
// vuelve a ligar lo que lo necesita: si el conjunto cambió, si el registro se
// reescribió (create/update, p. ej. Force tras borrar en el destino) o si nunca
// se alcanzó a ligar (el destino aún no tenía al dueño, ErrAttachDeferred, o
// Attach falló). Ligar sigue siendo idempotente: MediaSink.Attach reemplaza.

// MediaSet es el conjunto vigente de un campo de un registro.
type MediaSet struct {
	Org    string `json:"org"`
	Entity string `json:"entity"`
	Key    string `json:"key"`
	Field  string `json:"field"`
	// URLs en orden, sin repetir; vacío = el campo ya no trae medios.
	URLs []string `json:"urls"`
	// Attached es la huella de las referencias ligadas por última vez para estas
	// URLs; vacía = falta ligar.
	Attached  string    `json:"attached,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MediaSetStore guarda los conjuntos vigentes; lo implementa ops/addons.
type MediaSetStore interface {
	// MediaSets devuelve los conjuntos guardados de esas identidades (todos sus campos).
	MediaSets(ctx context.Context, org, entity string, keys []string) ([]MediaSet, error)
	// PutMediaSet reemplaza las URLs de (Org, Entity, Key, Field). Si las URLs
	// cambian, Attached queda vacío; si son las mismas, Attached se conserva.
	PutMediaSet(ctx context.Context, s MediaSet) error
	// MarkAttached deja Attached = hash solo si las URLs guardadas siguen siendo
	// urls (atómico: un Sync que cambió el conjunto mientras tanto gana).
	MarkAttached(ctx context.Context, org, entity, key, field string, urls []string, hash string) error
}

// ErrAttachDeferred lo devuelve un MediaSink cuando todavía no puede ligar (p.
// ej. el registro dueño aún no existe o está incompleto en el destino). No es
// un fallo: el campo queda pendiente de ligar y el siguiente Sync que vea esa
// identidad lo vuelve a intentar.
var ErrAttachDeferred = newErr(CodeMediaAttachDeferred)

// MediaRow son los medios que la escritura de una identidad deja vigentes.
type MediaRow struct {
	Key   string
	Media map[string][]string
	// Rewritten: el registro se acaba de crear o actualizar en el destino; sus
	// medios se vuelven a ligar aunque el conjunto no haya cambiado.
	Rewritten bool
}

// uniqueURLs quita repetidos conservando el orden de la primera aparición.
func uniqueURLs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, u := range in {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func sameURLs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// refsHash es la huella de un conjunto de referencias ligado.
func refsHash(refs []MediaRef) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n", len(refs))
	for _, r := range refs {
		fmt.Fprintf(h, "%s|%s\n", r.SourceURL, r.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Sync deja vigentes los medios de rows: guarda el conjunto de cada campo,
// encola las descargas que falten (idempotente por URL) y vuelve a ligar los
// campos que lo necesitan y ya no esperan descargas. Devuelve cuántos trabajos
// eran nuevos. Sin Sets se comporta como antes (encola y liga el historial).
func (q *MediaQueue) Sync(ctx context.Context, org, entity, runID string, rows []MediaRow) (int, error) {
	var jobs []MediaJob
	for _, row := range rows {
		jobs = append(jobs, q.jobsFor(org, entity, runID, row.Key, row.Media)...)
	}
	if q.Sets == nil {
		if len(jobs) == 0 {
			return 0, nil
		}
		return q.Jobs.Enqueue(ctx, jobs)
	}
	type fieldKey struct{ key, field string }
	current := map[fieldKey]MediaSet{}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Key)
	}
	for lo := 0; lo < len(keys); lo += DefaultLookupChunk {
		got, err := q.Sets.MediaSets(ctx, org, entity, keys[lo:min(lo+DefaultLookupChunk, len(keys))])
		if err != nil {
			return 0, wrapErr(err, CodeMediaSetStore)
		}
		for _, s := range got {
			current[fieldKey{s.Key, s.Field}] = s
		}
	}
	type pendingAttach struct {
		set   MediaSet
		force bool
	}
	var attach []pendingAttach
	for _, row := range rows {
		fields := map[string]bool{}
		for f := range row.Media {
			fields[f] = true
		}
		for fk := range current {
			if fk.key == row.Key {
				fields[fk.field] = true
			}
		}
		names := make([]string, 0, len(fields))
		for f := range fields {
			names = append(names, f)
		}
		sort.Strings(names)
		for _, f := range names {
			urls := uniqueURLs(row.Media[f])
			cur, had := current[fieldKey{row.Key, f}]
			if !had && len(urls) == 0 {
				continue
			}
			set := MediaSet{Org: org, Entity: entity, Key: row.Key, Field: f, URLs: urls, UpdatedAt: q.now()}
			changed := !had || !sameURLs(cur.URLs, urls)
			if changed {
				if err := q.Sets.PutMediaSet(ctx, set); err != nil {
					return 0, wrapErr(err, CodeMediaSetStore)
				}
			} else {
				set.Attached = cur.Attached
			}
			if changed || row.Rewritten || set.Attached == "" {
				attach = append(attach, pendingAttach{set, row.Rewritten})
			}
		}
	}
	added := 0
	if len(jobs) > 0 {
		n, err := q.Jobs.Enqueue(ctx, jobs)
		if err != nil {
			return 0, err
		}
		added = n
	}
	for _, a := range attach {
		if err := q.attachSet(ctx, a.set, a.force); err != nil {
			return added, err
		}
	}
	return added, nil
}

// attachSet liga el conjunto vigente de un campo si ya ninguna de sus URLs
// espera descarga. Sin force no vuelve a ligar lo que ya está ligado igual.
// Las URLs cuyo trabajo murió (o que la política rechazó) no aparecen.
func (q *MediaQueue) attachSet(ctx context.Context, set MediaSet, force bool) error {
	if q.Sink == nil {
		return nil
	}
	jobs, err := q.Jobs.List(ctx, set.Org, MediaJobFilter{Entity: set.Entity, Key: set.Key, Field: set.Field})
	if err != nil {
		return err
	}
	byURL := make(map[string]MediaJob, len(jobs))
	for _, j := range jobs {
		byURL[j.URL] = j
	}
	refs := []MediaRef{}
	for _, u := range set.URLs {
		j, ok := byURL[u]
		if !ok {
			continue
		}
		switch j.State {
		case MediaPending, MediaRunning:
			return nil // se liga cuando termine el último
		case MediaDone:
			e, ok, err := q.Backup.Manifest.ByURL(ctx, set.Org, u)
			if err != nil {
				return err
			}
			if ok {
				refs = append(refs, MediaRef{Index: len(refs), SourceURL: u, SHA256: e.SHA256, StoredName: e.StoredName, ContentType: e.ContentType, Bytes: e.Bytes})
			}
		}
	}
	hash := refsHash(refs)
	if !force && set.Attached == hash {
		return nil
	}
	if err := q.Sink.Attach(ctx, set.Org, set.Entity, set.Key, set.Field, refs); err != nil {
		if errors.Is(err, ErrAttachDeferred) {
			return nil // queda Attached vacío: el siguiente Sync lo intenta otra vez
		}
		// Se olvida lo ligado antes: el siguiente Sync vuelve a intentarlo.
		_ = q.Sets.MarkAttached(context.WithoutCancel(ctx), set.Org, set.Entity, set.Key, set.Field, set.URLs, "")
		return wrapErr(err, CodeMediaAttach, "key", set.Key, "field", set.Field)
	}
	if err := q.Sets.MarkAttached(ctx, set.Org, set.Entity, set.Key, set.Field, set.URLs, hash); err != nil {
		return wrapErr(err, CodeMediaSetStore)
	}
	return nil
}

// attachJob liga, tras terminar un trabajo, los conjuntos vigentes que incluyen
// su URL. Sin Sets usa el historial de la cola (comportamiento anterior).
func (q *MediaQueue) attachJob(ctx context.Context, j MediaJob) error {
	if q.Sink == nil {
		return nil
	}
	if q.Sets == nil {
		return q.attachIfSettled(ctx, j)
	}
	sets, err := q.Sets.MediaSets(ctx, j.Org, j.Entity, []string{j.Key})
	if err != nil {
		return wrapErr(err, CodeMediaSetStore)
	}
	for _, s := range sets {
		if s.Field != j.Field {
			continue
		}
		for _, u := range s.URLs {
			if u == j.URL {
				return q.attachSet(ctx, s, false)
			}
		}
		return nil // la URL ya no es vigente: no cambia lo ligado
	}
	// El campo no tiene conjunto guardado (el trabajo se encoló sin Sync): se
	// liga con el historial, como sin MediaSetStore.
	return q.attachIfSettled(ctx, j)
}

// MemoryMediaSetStore es un MediaSetStore en memoria (pruebas y desarrollo).
type MemoryMediaSetStore struct {
	mu sync.Mutex
	m  map[string]MediaSet
}

func NewMemoryMediaSetStore() *MemoryMediaSetStore {
	return &MemoryMediaSetStore{m: map[string]MediaSet{}}
}

func (s *MemoryMediaSetStore) MediaSets(_ context.Context, org, entity string, keys []string) ([]MediaSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var out []MediaSet
	for _, v := range s.m {
		if v.Org == org && v.Entity == entity && want[v.Key] {
			v.URLs = append([]string(nil), v.URLs...)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key+"\x00"+out[a].Field < out[b].Key+"\x00"+out[b].Field })
	return out, nil
}

func (s *MemoryMediaSetStore) PutMediaSet(_ context.Context, set MediaSet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := mk(set.Org, set.Entity, set.Key, set.Field)
	set.URLs = append([]string(nil), set.URLs...)
	if prev, ok := s.m[k]; ok && sameURLs(prev.URLs, set.URLs) {
		set.Attached = prev.Attached
	} else {
		set.Attached = ""
	}
	s.m[k] = set
	return nil
}

func (s *MemoryMediaSetStore) MarkAttached(_ context.Context, org, entity, key, field string, urls []string, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := mk(org, entity, key, field)
	if prev, ok := s.m[k]; ok && sameURLs(prev.URLs, urls) {
		prev.Attached = hash
		s.m[k] = prev
	}
	return nil
}
