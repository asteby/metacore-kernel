package sqlpoll

import (
	"context"
	"fmt"
	"strings"
	"time"

	lb "github.com/asteby/metacore-kernel/legacybridge"
)

// Ingester aplica un lote en el host (*legacybridge.Applier, o el envoltorio del
// host, o un cliente HTTP del endpoint de ingesta cuando el adaptador corre
// junto al legado).
type Ingester interface {
	Ingest(ctx context.Context, org string, b lb.Batch) (lb.IngestResult, error)
}

// CursorReader lee el cursor guardado en el host.
type CursorReader interface {
	GetCursor(ctx context.Context, org, system, stream string) (string, error)
}

// RefReader es lo que Reconcile lee de bridge_refs.
type RefReader interface {
	GetRef(ctx context.Context, org, system, entity, externalID string) (lb.Ref, bool, error)
	ListRefs(ctx context.Context, org, system, entity, after string, limit int) ([]lb.Ref, error)
}

// StreamReport resume una corrida de un flujo.
type StreamReport struct {
	Stream  string             `json:"stream"`
	Batches int                `json:"batches"`
	Counts  map[lb.Outcome]int `json:"counts"`
	Cursor  string             `json:"cursor"`
	// Lag es ahora menos el instante del cursor (cero sin cursor).
	Lag   time.Duration `json:"lag"`
	Error string        `json:"error,omitempty"`
}

// MaxRounds acota los lotes de un flujo en una corrida de Sync.
const MaxRounds = 200

// Sync lee e ingiere los cambios de cada flujo con updated_at (o solo los de
// streams) hasta alcanzar el límite de asentamiento. El cursor lo guarda el
// host; Sync lo vuelve a leer antes de cada lote, así una corrida simultánea
// que ya avanzó no se repite y una caída a medio lote se retoma desde el último
// cursor guardado (lo ya aplicado vuelve como duplicado).
func (s *Source) Sync(ctx context.Context, org string, cursors CursorReader, ing Ingester, streams ...string) ([]StreamReport, error) {
	want := map[string]bool{}
	for _, n := range streams {
		want[n] = true
	}
	var out []StreamReport
	for _, st := range s.Profile.Streams {
		if st.UpdatedAt == "" || (len(want) > 0 && !want[st.Name]) {
			continue
		}
		rep := StreamReport{Stream: st.Name, Counts: map[lb.Outcome]int{}}
		err := s.syncStream(ctx, org, st, cursors, ing, &rep)
		if err != nil {
			rep.Error = err.Error()
		}
		if t, _, ok, _ := decodeCursor(st, rep.Cursor); ok {
			rep.Lag = s.now().Sub(t)
		}
		out = append(out, rep)
		if err != nil && ctx.Err() != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Source) syncStream(ctx context.Context, org string, st Stream, cursors CursorReader, ing Ingester, rep *StreamReport) error {
	for round := 0; round < MaxRounds; round++ {
		cur, err := cursors.GetCursor(ctx, org, s.Profile.System, st.Name)
		if err != nil {
			return err
		}
		rep.Cursor = cur
		b, err := s.Poll(ctx, st.Name, cur)
		if err != nil {
			return err
		}
		if len(b.Envelopes) == 0 {
			return nil
		}
		res, err := ing.Ingest(ctx, org, b)
		if err != nil {
			return err
		}
		if !res.Advanced && lb.CompareCursors(res.Cursor, b.Cursor) < 0 && res.Counts[lb.OutcomeError] == 0 {
			// Ni este lote ni otra corrida movieron el cursor hasta el del lote.
			return fmt.Errorf("%w: %s quedó en %s y el lote llegaba a %s", ErrCursorStalled, st.Name, res.Cursor, b.Cursor)
		}
		rep.Batches++
		for k, v := range res.Counts {
			rep.Counts[k] += v
		}
		rep.Cursor = res.Cursor
		if res.Counts[lb.OutcomeError] > 0 {
			// El cursor no avanzó: el siguiente ciclo reintenta el lote.
			return fmt.Errorf("sqlpoll: %s: %d envelopes con falla de infraestructura", st.Name, res.Counts[lb.OutcomeError])
		}
		if len(b.Envelopes) < s.pageSize() {
			return nil
		}
	}
	return nil
}

// ReconcileReport resume la comparación de un flujo contra bridge_refs.
type ReconcileReport struct {
	Stream  string `json:"stream"`
	Scanned int    `json:"scanned"`
	InSync  int    `json:"in_sync"`
	// Missing: filas del legado sin bridge_refs (o borradas en el host).
	Missing int `json:"missing"`
	// Changed: la huella difiere (p. ej. un UPDATE que no tocó updated_at).
	Changed int `json:"changed"`
	// Deleted: refs vivos cuya llave ya no existe en el legado.
	Deleted int `json:"deleted"`
	Emitted int `json:"emitted"`
}

// Diffs es lo que la reconciliación tuvo que corregir.
func (r ReconcileReport) Diffs() int { return r.Missing + r.Changed + r.Deleted }

// Reconcile recorre la tabla del flujo por llave y la compara con bridge_refs
// del host. Emite (por emit, en lotes sin cursor: no mueve el del flujo) los
// envelopes de lo que falta o cambió y op=delete para las llaves borradas en el
// legado. Antes de borrar vuelve a comprobar la llave en el legado, para no
// borrar una fila que se creó durante la corrida.
func (s *Source) Reconcile(ctx context.Context, org, stream string, refs RefReader, emit func(lb.Batch) error) (ReconcileReport, error) {
	st, ok := s.Profile.Stream(stream)
	if !ok {
		return ReconcileReport{}, fmt.Errorf("sqlpoll: flujo %q no está en el perfil", stream)
	}
	rep := ReconcileReport{Stream: st.Name}
	d := s.Dialect
	key := d.keyExpr(st)
	page := s.pageSize()
	pending := lb.Batch{Source: s.Profile.System, Stream: st.Name}
	flush := func(force bool) error {
		if len(pending.Envelopes) == 0 || (!force && len(pending.Envelopes) < page) {
			return nil
		}
		rep.Emitted += len(pending.Envelopes)
		err := emit(pending)
		pending.Envelopes = nil
		return err
	}
	seen := map[string]bool{}
	var after any
	for {
		ph := &placeholders{d: d}
		q := "SELECT " + s.selectList(st) + " FROM " + d.quote(st.Table)
		var args []any
		if after != nil {
			q += " WHERE " + key + " > " + ph.next()
			args = append(args, after)
		}
		q += " ORDER BY " + key + " LIMIT " + ph.next()
		args = append(args, page)
		rows, err := s.query(ctx, q, args...)
		if err != nil {
			return rep, fmt.Errorf("sqlpoll: reconciliar %s: %w", st.Table, err)
		}
		for _, r := range rows {
			env, err := s.envelope(st, r)
			if err != nil {
				return rep, err
			}
			rep.Scanned++
			seen[env.ExternalID] = true
			ref, found, err := refs.GetRef(ctx, org, s.Profile.System, st.Entity, env.ExternalID)
			if err != nil {
				return rep, err
			}
			switch {
			case !found || ref.State == lb.RefDeleted:
				rep.Missing++
			case ref.LastHash != env.Hash():
				rep.Changed++
			default:
				rep.InSync++
				continue
			}
			pending.Envelopes = append(pending.Envelopes, env)
			if err := flush(false); err != nil {
				return rep, err
			}
		}
		if len(rows) < page {
			break
		}
		after = rows[len(rows)-1][st.Key]
	}

	prefix := s.Profile.System + ":" + st.Table + ":"
	deleteVersion := s.now().UTC().Format(time.RFC3339Nano) + "#delete"
	cursor := ""
	for {
		list, err := refs.ListRefs(ctx, org, s.Profile.System, st.Entity, cursor, lb.MaxListRefs)
		if err != nil {
			return rep, err
		}
		for _, ref := range list {
			if seen[ref.ExternalID] || ref.State != lb.RefSynced || !strings.HasPrefix(ref.ExternalID, prefix) {
				continue
			}
			exists, err := s.exists(ctx, st, strings.TrimPrefix(ref.ExternalID, prefix))
			if err != nil {
				return rep, err
			}
			if exists {
				continue
			}
			rep.Deleted++
			pending.Envelopes = append(pending.Envelopes, lb.Envelope{Source: s.Profile.System, Entity: st.Entity,
				ExternalID: ref.ExternalID, Op: lb.OpDelete, SourceVersion: deleteVersion})
			if err := flush(false); err != nil {
				return rep, err
			}
		}
		if len(list) < lb.MaxListRefs {
			break
		}
		cursor = list[len(list)-1].ExternalID
	}
	return rep, flush(true)
}

func (s *Source) exists(ctx context.Context, st Stream, key string) (bool, error) {
	ph := &placeholders{d: s.Dialect}
	rows, err := s.query(ctx, "SELECT "+s.Dialect.quote(st.Key)+" FROM "+s.Dialect.quote(st.Table)+
		" WHERE "+s.Dialect.keyExpr(st)+" = "+ph.next()+" LIMIT 1", key)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}
