package diag

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// EnvSwitch es la variable que apaga la capacidad: KERNEL_DIAGNOSTICS=off
// (tambien 0/false/disabled). Sin ella, la captura esta encendida.
const EnvSwitch = "KERNEL_DIAGNOSTICS"

// EnabledFromEnv lee EnvSwitch.
func EnabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvSwitch))) {
	case "off", "0", "false", "disabled", "no":
		return false
	}
	return true
}

// Alert es lo que recibe un Alerter cuando un grupo merece atencion.
type Alert struct {
	Org         uuid.UUID
	GroupID     uuid.UUID
	AddonKey    string
	Operation   string
	Kind        Kind
	Code        string
	Severity    Severity
	HTTPStatus  int
	Fingerprint string
	// Reason: "new_group" (grupo nuevo de severidad error o mayor) o "burst"
	// (BurstThreshold fallos en BurstWindow).
	Reason string
	// Count es el total acumulado del grupo; Recent los fallos en la ventana.
	Count  int64
	Recent int
}

// Alerter abre alertas. Detras de esta interfaz para no acoplar el Recorder a
// ningun paquete concreto (ver IncidentsAlerter).
type Alerter interface {
	Alert(ctx context.Context, a Alert) error
}

// Options del Recorder; todo es opcional.
type Options struct {
	// Enabled fuerza el interruptor (nil = EnabledFromEnv()).
	Enabled *bool
	// FlushEvery: cada cuanto se escribe el agregado (5s).
	FlushEvery time.Duration
	// RatePerMin: muestras por minuto por org x addon (60). Sobre el tope solo
	// sube el contador del grupo.
	RatePerMin int
	// MaxPending: grupos distintos pendientes de escribir (2000); sobre eso se
	// descartan los nuevos (el guest nunca espera).
	MaxPending int
	// BurstThreshold / BurstWindow: alerta por rafaga (5 en 10 min).
	BurstThreshold int
	BurstWindow    time.Duration
	Alerter        Alerter
	// AddonVersion resuelve la version instalada de un addon (opcional).
	AddonVersion func(addonKey string) string
	Logger       *slog.Logger
	Now          func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

type pendingEntry struct {
	d       Diagnostic // representativo (saneado)
	fp      string
	gid     uuid.UUID
	count   int64
	last    time.Time
	samples []Diagnostic
}

type burstState struct {
	times   []time.Time
	alerted time.Time
}

// Recorder agrega y escribe diagnosticos de forma asincrona: Record nunca
// bloquea ni falla. Un *Recorder nil es valido y no hace nada.
type Recorder struct {
	store *Store
	opt   Options
	on    bool
	log   *slog.Logger

	mu       sync.Mutex
	pending  map[string]*pendingEntry // org|fp
	buckets  map[string]*bucket       // org|addon
	bursts   map[string]*burstState   // org|fp
	burstDue map[string]struct{}      // org|fp pendientes de alertar
	dropped  int64

	startOnce sync.Once
	closeOnce sync.Once
	started   atomic.Bool
	stop      chan struct{}
	done      chan struct{}
	lastPurge time.Time
}

// NewRecorder crea el Recorder. store nil o interruptor off => inerte.
func NewRecorder(store *Store, opt Options) *Recorder {
	if opt.FlushEvery <= 0 {
		opt.FlushEvery = 5 * time.Second
	}
	if opt.RatePerMin <= 0 {
		opt.RatePerMin = 60
	}
	if opt.MaxPending <= 0 {
		opt.MaxPending = 2000
	}
	if opt.BurstThreshold <= 0 {
		opt.BurstThreshold = 5
	}
	if opt.BurstWindow <= 0 {
		opt.BurstWindow = 10 * time.Minute
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	on := EnabledFromEnv()
	if opt.Enabled != nil {
		on = *opt.Enabled
	}
	lg := opt.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Recorder{
		store: store, opt: opt, on: on && store != nil, log: lg.With("component", "diag"),
		pending: map[string]*pendingEntry{}, buckets: map[string]*bucket{},
		bursts: map[string]*burstState{}, burstDue: map[string]struct{}{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Enabled reporta si la captura esta activa.
func (r *Recorder) Enabled() bool { return r != nil && r.on }

// VersionOf devuelve la version instalada del addon, si el host la conoce.
func (r *Recorder) VersionOf(addonKey string) string {
	if r == nil || r.opt.AddonVersion == nil {
		return ""
	}
	return r.opt.AddonVersion(addonKey)
}

// Record sanea d (con los secretos de sc) y lo encola. No bloquea, no hace E/S
// y nunca devuelve error: un fallo de diagnostico no puede romper al guest.
func (r *Recorder) Record(d Diagnostic, sc *Scrubber) {
	if !r.Enabled() {
		return
	}
	defer func() { _ = recover() }()
	if d.Org == uuid.Nil {
		return
	}
	if d.AddonVersion == "" {
		d.AddonVersion = r.VersionOf(d.AddonKey)
	}
	if d.At.IsZero() {
		d.At = r.opt.Now()
	}
	org := d.Org
	d = Sanitize(d, sc)
	if d.AddonKey == "" {
		return
	}
	if d.InvocationID != "" && d.DiagRef == "" {
		d.DiagRef = RefFor(org, d.InvocationID)
	}
	fp := Fingerprint(d)
	key := org.String() + "|" + fp
	now := r.opt.Now()

	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.pending[key]
	if e == nil {
		if len(r.pending) >= r.opt.MaxPending {
			r.dropped++
			return
		}
		e = &pendingEntry{d: d, fp: fp, gid: GroupID(org, fp)}
		r.pending[key] = e
	}
	e.count++
	if d.At.After(e.last) {
		e.last = d.At
		e.d.Severity, e.d.AddonVersion = d.Severity, d.AddonVersion
	}
	if len(e.samples) < 5 && r.take(org.String()+"|"+d.AddonKey, now) {
		e.samples = append(e.samples, d)
	}
	r.noteBurst(key, now, d, e)
}

// take: token bucket por org x addon.
func (r *Recorder) take(k string, now time.Time) bool {
	b := r.buckets[k]
	cap := float64(r.opt.RatePerMin)
	if b == nil {
		if len(r.buckets) > 10000 { // acotado: los inactivos se pierden sin dano
			r.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: cap, last: now}
		r.buckets[k] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * cap / 60
	if b.tokens > cap {
		b.tokens = cap
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *Recorder) noteBurst(key string, now time.Time, d Diagnostic, e *pendingEntry) {
	if r.opt.Alerter == nil || d.Severity.rank() < SeverityError.rank() {
		return
	}
	st := r.bursts[key]
	if st == nil {
		if len(r.bursts) > 10000 {
			r.bursts = map[string]*burstState{}
		}
		st = &burstState{}
		r.bursts[key] = st
	}
	cut := now.Add(-r.opt.BurstWindow)
	kept := st.times[:0]
	for _, t := range st.times {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	st.times = append(kept, now)
	if len(st.times) >= r.opt.BurstThreshold && st.alerted.Before(cut) {
		st.alerted = now
		r.burstDue[key] = struct{}{}
	}
}

// Start lanza el bucle de escritura (flush cada FlushEvery y purga diaria). Es
// idempotente. Close lo detiene con un ultimo flush.
func (r *Recorder) Start(ctx context.Context) {
	if !r.Enabled() {
		return
	}
	r.startOnce.Do(func() {
		r.started.Store(true)
		go func() {
			defer close(r.done)
			t := time.NewTicker(r.opt.FlushEvery)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					r.tick(ctx)
				case <-r.stop:
					r.Flush(context.Background())
					return
				case <-ctx.Done():
					r.Flush(context.Background())
					return
				}
			}
		}()
	})
}

// Close detiene el bucle (si estaba en marcha) y vuelca lo pendiente.
func (r *Recorder) Close() {
	if !r.Enabled() {
		return
	}
	r.closeOnce.Do(func() { close(r.stop) })
	if r.started.Load() {
		<-r.done
	}
	r.Flush(context.Background())
}

func (r *Recorder) tick(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("diag.tick_panic", "panic", p)
		}
	}()
	r.Flush(ctx)
	if now := r.opt.Now(); now.Sub(r.lastPurge) >= 24*time.Hour {
		r.lastPurge = now
		if _, _, err := r.PurgeNow(ctx); err != nil {
			r.log.Warn("diag.purge_failed", "err", err.Error())
		}
	}
}

// PurgeNow aplica la retencion (eventos 14 dias, grupos 90).
func (r *Recorder) PurgeNow(ctx context.Context) (events, groups int64, err error) {
	if !r.Enabled() {
		return 0, 0, nil
	}
	return r.store.purge(ctx, r.opt.Now(), EventRetention, GroupRetention)
}

// Dropped devuelve cuantos grupos nuevos se descartaron por saturacion.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

type applied struct {
	e     *pendingEntry
	org   uuid.UUID
	isNew bool
	count int64
}

// Flush escribe lo agregado. Si la escritura falla, lo devuelve a la cola (con
// tope) para el siguiente intento. Seguro para llamar concurrentemente.
func (r *Recorder) Flush(ctx context.Context) {
	if !r.Enabled() {
		return
	}
	r.mu.Lock()
	batch := r.pending
	r.pending = map[string]*pendingEntry{}
	due := r.burstDue
	r.burstDue = map[string]struct{}{}
	r.mu.Unlock()

	if len(batch) > 0 {
		res, err := r.store.apply(ctx, batch)
		if err != nil {
			r.log.Warn("diag.flush_failed", "err", err.Error(), "groups", len(batch))
			r.requeue(batch, due)
			return
		}
		r.alert(ctx, res, due)
	}
}

func (r *Recorder) requeue(batch map[string]*pendingEntry, due map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range batch {
		if cur := r.pending[k]; cur != nil {
			cur.count += e.count
			continue
		}
		if len(r.pending) < r.opt.MaxPending {
			r.pending[k] = e
		} else {
			r.dropped++
		}
	}
	for k := range due {
		r.burstDue[k] = struct{}{}
	}
}

func (r *Recorder) alert(ctx context.Context, res []applied, due map[string]struct{}) {
	if r.opt.Alerter == nil {
		return
	}
	for _, a := range res {
		key := a.org.String() + "|" + a.e.fp
		reason := ""
		recent := 0
		if _, ok := due[key]; ok {
			reason, recent = "burst", r.opt.BurstThreshold
		} else if a.isNew && a.e.d.Severity.rank() >= SeverityError.rank() {
			reason, recent = "new_group", int(a.e.count)
		}
		if reason == "" {
			continue
		}
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					r.log.Error("diag.alerter_panic", "panic", p)
				}
			}()
			return r.opt.Alerter.Alert(ctx, Alert{
				Org: a.org, GroupID: a.e.gid, AddonKey: a.e.d.AddonKey, Operation: a.e.d.Operation, Kind: a.e.d.Kind,
				Code: a.e.d.Code, Severity: a.e.d.Severity, HTTPStatus: a.e.d.HTTPStatus, Fingerprint: a.e.fp,
				Reason: reason, Count: a.count, Recent: recent,
			})
		}()
		if err != nil {
			r.log.Warn("diag.alert_failed", "err", err.Error(), "group", a.e.gid.String())
		}
	}
}

// apply escribe el lote en una transaccion propia (fuera de la tx del guest).
func (s *Store) apply(ctx context.Context, batch map[string]*pendingEntry) ([]applied, error) {
	pg := s.db.Dialector.Name() == "postgres"
	var out []applied
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		out = out[:0]
		for _, e := range batch {
			d := e.d
			org := d.Org
			var lastEvent *uuid.UUID
			var evIDs []uuid.UUID
			for range e.samples {
				evIDs = append(evIDs, uuid.New())
			}
			if len(evIDs) > 0 {
				id := evIDs[len(evIDs)-1]
				lastEvent = &id
			}
			first := e.last
			if len(e.samples) > 0 {
				first = e.samples[0].At
			}
			if first.After(e.last) || first.IsZero() {
				first = e.last
			}
			var ret struct {
				Count  int64
				Status string
			}
			err := tx.Raw(`INSERT INTO `+TableGroups+` (id, org_id, addon_key, addon_version, operation, kind, code, severity, http_status, fingerprint, count, first_seen_at, last_seen_at, status, last_event_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?)
ON CONFLICT (org_id, fingerprint) DO UPDATE SET
  count = `+TableGroups+`.count + EXCLUDED.count,
  last_seen_at = CASE WHEN EXCLUDED.last_seen_at > `+TableGroups+`.last_seen_at THEN EXCLUDED.last_seen_at ELSE `+TableGroups+`.last_seen_at END,
  addon_version = EXCLUDED.addon_version,
  severity = EXCLUDED.severity,
  status = CASE WHEN `+TableGroups+`.status = 'resolved' THEN 'open' ELSE `+TableGroups+`.status END,
  last_event_id = COALESCE(EXCLUDED.last_event_id, `+TableGroups+`.last_event_id)
RETURNING count, status`,
				e.gid, org, d.AddonKey, d.AddonVersion, d.Operation, string(d.Kind), d.Code, string(d.Severity), d.HTTPStatus,
				e.fp, e.count, first.UTC(), e.last.UTC(), lastEvent).Scan(&ret).Error
			if err != nil {
				return err
			}
			isNew := ret.Count == e.count
			for i, smp := range e.samples {
				payload, _ := json.Marshal(smp)
				ph := "?"
				if pg {
					ph = "?::jsonb"
				}
				if err := tx.Exec(`INSERT INTO `+TableEvents+` (id, org_id, group_id, correlation_id, invocation_id, diag_ref, subject_type, subject_id, is_first, occurred_at, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+ph+`)`,
					evIDs[i], org, e.gid, smp.CorrelationID, smp.InvocationID, smp.DiagRef, smp.Subject.Type, smp.Subject.ID,
					isNew && i == 0, smp.At.UTC(), string(payload)).Error; err != nil {
					return err
				}
			}
			if len(evIDs) > 0 {
				if err := tx.Exec(`DELETE FROM `+TableEvents+` WHERE org_id = ? AND group_id = ? AND is_first = ? AND id NOT IN (
SELECT id FROM `+TableEvents+` WHERE org_id = ? AND group_id = ? AND is_first = ? ORDER BY occurred_at DESC, id LIMIT ?)`,
					org, e.gid, false, org, e.gid, false, MaxSamplesPerGroup).Error; err != nil {
					return err
				}
			}
			out = append(out, applied{e: e, org: org, isNew: isNew, count: ret.Count})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
