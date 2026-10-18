package legacybridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/outbox"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// PumpKind es el tipo de job de outbox que entrega la cola de un agregado.
const PumpKind = "legacybridge.pump"

// maxCommandError acota last_error.
const maxCommandError = 2048

// Delivery es la respuesta del legado a un comando entregado.
type Delivery struct {
	// ExternalID y SourceVersion (opcionales): el id y la versión que el legado
	// asignó. Si el comando trae Entity y LocalID se registran en bridge_refs
	// para que el eco de la réplica solo confirme el mapeo.
	ExternalID    string          `json:"external_id,omitempty"`
	SourceVersion string          `json:"source_version,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
}

// Connector entrega un comando al legado. Debe ser idempotente por
// Command.IdempotencyKey. Un error envuelto con Permanent no se reintenta; uno
// envuelto con RetryAfter elige la espera.
type Connector interface {
	Send(ctx context.Context, c Command) (Delivery, error)
}

// ConnectorFunc adapta una función a Connector.
type ConnectorFunc func(ctx context.Context, c Command) (Delivery, error)

func (f ConnectorFunc) Send(ctx context.Context, c Command) (Delivery, error) { return f(ctx, c) }

type permanentErr struct{ err error }

func (e permanentErr) Error() string { return e.err.Error() }
func (e permanentErr) Unwrap() error { return e.err }

// Permanent marca un error de entrega que no se reintenta (4xx de validación).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentErr{err}
}

// IsPermanent reporta si err se marcó con Permanent.
func IsPermanent(err error) bool {
	var p permanentErr
	return errors.As(err, &p)
}

type retryAfterErr struct {
	err   error
	delay time.Duration
}

func (e retryAfterErr) Error() string { return e.err.Error() }
func (e retryAfterErr) Unwrap() error { return e.err }

// RetryAfter pide reintentar tras delay (p. ej. un 429 con Retry-After).
func RetryAfter(err error, delay time.Duration) error {
	if err == nil {
		return nil
	}
	return retryAfterErr{err, delay}
}

// Exponential es un backoff con jitter: el intento n espera ~base·2^(n-1), con tope max.
func Exponential(base, max time.Duration) func(int) time.Duration {
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		d := base
		for i := 1; i < attempt && d < max; i++ {
			d *= 2
		}
		if d > max {
			d = max
		}
		// jitter en [d/2, d]
		return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	}
}

// OutboundConfig arma un Outbound.
type OutboundConfig struct {
	Store     Store
	Outbox    *outbox.Service
	Connector Connector
	// MaxAttempts por comando antes de ir a muertos (por omisión 25).
	MaxAttempts int
	// MaxAge: un comando que no se entrega en este tiempo va a muertos (24 h).
	MaxAge  time.Duration
	Backoff func(attempt int) time.Duration
	// OnDead se llama cuando un comando pasa a muertos (alertas).
	OnDead func(ctx context.Context, c Command)
	Logger *slog.Logger
	Now    func() time.Time
}

// Outbound guarda comandos hacia el legado y los entrega en orden por agregado.
type Outbound struct{ cfg OutboundConfig }

// NewOutbound valida la configuración. Llama a Register antes de Outbox.Start.
func NewOutbound(cfg OutboundConfig) (*Outbound, error) {
	if cfg.Store == nil || cfg.Outbox == nil || cfg.Connector == nil {
		return nil, errors.New("legacybridge: NewOutbound requiere Store, Outbox y Connector")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 25
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 24 * time.Hour
	}
	if cfg.Backoff == nil {
		cfg.Backoff = Exponential(10*time.Second, 30*time.Minute)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Logger = cfg.Logger.With("component", "legacybridge.outbound")
	return &Outbound{cfg: cfg}, nil
}

// Register registra el handler de la bomba en el outbox.
func (o *Outbound) Register() { o.cfg.Outbox.Register(PumpKind, o.pump) }

type pumpPayload struct {
	Org          string `json:"org"`
	System       string `json:"system"`
	AggregateKey string `json:"aggregate_key"`
}

// Publish guarda el comando y despierta la bomba de su agregado. Si ctx trae
// una transacción (WithTx / GormAtomic), los dos quedan en ella: el comando se
// confirma junto con la fila de negocio que lo originó. Una IdempotencyKey
// repetida devuelve el comando ya guardado.
func (o *Outbound) Publish(ctx context.Context, c Command) (Command, error) {
	if c.Org == "" || c.System == "" || c.Kind == "" || c.IdempotencyKey == "" {
		return Command{}, errors.New("legacybridge: comando sin org, system, kind o idempotency_key")
	}
	if c.AggregateKey == "" {
		c.AggregateKey = c.Kind + ":" + c.IdempotencyKey
	}
	if len(c.Payload) == 0 {
		c.Payload = json.RawMessage(`{}`)
	}
	saved, created, err := o.cfg.Store.InsertCommand(ctx, c)
	if err != nil || !created {
		return saved, err
	}
	return saved, o.kick(ctx, AggregateRef{Org: saved.Org, System: saved.System, AggregateKey: saved.AggregateKey}, time.Time{})
}

func (o *Outbound) kick(ctx context.Context, a AggregateRef, at time.Time) error {
	opts := []outbox.EnqueueOption{
		outbox.WithDedupeKey(a.Org + "|" + a.System + "|" + a.AggregateKey),
		// Los reintentos los cuenta el comando, no el job.
		outbox.WithMaxAttempts(1 << 20),
	}
	if id, err := uuid.Parse(a.Org); err == nil {
		opts = append(opts, outbox.WithOrg(id))
	}
	if !at.IsZero() {
		opts = append(opts, outbox.WithRunAt(at))
	}
	var err error
	if tx, ok := TxFrom(ctx); ok {
		_, err = o.cfg.Outbox.EnqueueTx(tx, PumpKind, pumpPayload(a), opts...)
	} else {
		_, err = o.cfg.Outbox.Enqueue(ctx, PumpKind, pumpPayload(a), opts...)
	}
	if errors.Is(err, outbox.ErrDuplicate) {
		return nil
	}
	return err
}

// pump entrega en orden los comandos del agregado. Un dead en la cabeza
// bloquea el agregado (el job termina; Sweep no lo vuelve a despertar).
func (o *Outbound) pump(ctx context.Context, job outbox.Job) error {
	var p pumpPayload
	if err := job.Decode(&p); err != nil {
		return outbox.Permanent(err)
	}
	a := AggregateRef(p)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, ok, err := o.cfg.Store.HeadCommand(ctx, a)
		if err != nil {
			return err
		}
		if !ok || c.Status != CommandPending {
			return nil
		}
		now := o.cfg.Now().UTC()
		if c.NextAttemptAt.After(now) {
			return outbox.RetryAfter(errors.New("legacybridge: comando en espera de reintento"), c.NextAttemptAt.Sub(now))
		}
		d, serr := o.cfg.Connector.Send(ctx, c)
		if serr == nil {
			if err := o.delivered(ctx, c, d); err != nil {
				return err
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return o.failed(ctx, c, serr)
	}
}

func (o *Outbound) delivered(ctx context.Context, c Command, d Delivery) error {
	now := o.cfg.Now().UTC()
	note := ""
	if c.Entity != "" && c.LocalID != "" && d.ExternalID != "" {
		ref, ok, err := o.cfg.Store.GetRefByLocal(ctx, c.Org, c.System, c.Entity, c.LocalID)
		if err != nil {
			return err
		}
		if !ok {
			ref = Ref{Org: c.Org, System: c.System, Entity: c.Entity, LocalID: c.LocalID, CreatedIn: OwnerLocal}
		}
		ref.ExternalID = d.ExternalID
		if d.SourceVersion != "" {
			ref.SourceVersion = d.SourceVersion
		}
		// La réplica que vuelva con esta versión solo confirma (RefPendingLegacy → synced).
		ref.State, ref.UpdatedAt = RefPendingLegacy, now
		if ref.Owner == "" {
			ref.Owner = OwnerLegacy
		}
		if err := o.cfg.Store.PutRef(ctx, ref); err != nil {
			if !errors.Is(err, ErrRefConflict) {
				return err
			}
			// El legado ya entregó; el duplicado lógico lo resuelve la conciliación.
			o.cfg.Logger.Warn("mapeo en conflicto tras entregar", "org", c.Org, "entity", c.Entity, "local_id", c.LocalID, "external_id", d.ExternalID)
			note = "entregado; " + err.Error()
		}
	}
	c.Status, c.LastError, c.UpdatedAt = CommandSent, note, now
	c.Attempts++
	if d.Body != nil {
		c.Response = d.Body
	} else {
		c.Response, _ = json.Marshal(d)
	}
	return o.cfg.Store.UpdateCommand(ctx, c)
}

func (o *Outbound) failed(ctx context.Context, c Command, serr error) error {
	now := o.cfg.Now().UTC()
	c.Attempts++
	c.LastError = truncate(serr.Error(), maxCommandError)
	c.UpdatedAt = now
	dead := IsPermanent(serr) || c.Attempts >= o.cfg.MaxAttempts || now.Sub(c.CreatedAt) >= o.cfg.MaxAge
	if dead {
		c.Status = CommandDead
		if err := o.cfg.Store.UpdateCommand(ctx, c); err != nil {
			return err
		}
		o.cfg.Logger.Warn("comando a muertos", "org", c.Org, "system", c.System, "kind", c.Kind, "id", c.ID, "attempts", c.Attempts, "err", c.LastError)
		if o.cfg.OnDead != nil {
			o.cfg.OnDead(ctx, c)
		}
		return nil
	}
	wait := o.cfg.Backoff(c.Attempts)
	var ra retryAfterErr
	if errors.As(serr, &ra) && ra.delay > 0 {
		wait = ra.delay
	}
	c.NextAttemptAt = now.Add(wait)
	if err := o.cfg.Store.UpdateCommand(ctx, c); err != nil {
		return err
	}
	return outbox.RetryAfter(serr, wait)
}

// Sweep despierta las bombas de los agregados con comandos vencidos (tras un
// reinicio, o si un comando llegó justo cuando su bomba terminaba). Llámalo
// periódicamente.
func (o *Outbound) Sweep(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	due, err := o.cfg.Store.DueAggregates(ctx, o.cfg.Now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	for _, a := range due {
		if err := o.kick(ctx, a, time.Time{}); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// Run llama a Sweep cada every hasta que ctx se cancela. Cubre la ventana en
// que un comando nuevo (o un RetryDead/Discard) encuentra la bomba de su
// agregado todavía en running y el outbox deduplica el despertar.
func (o *Outbound) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := o.Sweep(ctx, 0); err != nil && ctx.Err() == nil {
				o.cfg.Logger.Warn("barrido de comandos", "err", err)
			}
		}
	}
}

// RetryDead regresa un comando muerto a la cola (con sus intentos en cero) y
// desbloquea su agregado.
func (o *Outbound) RetryDead(ctx context.Context, id string) (Command, error) {
	c, ok, err := o.cfg.Store.GetCommand(ctx, id)
	if err != nil {
		return c, err
	}
	if !ok {
		return c, ErrCommandNotFound
	}
	if c.Status != CommandDead {
		return c, fmt.Errorf("legacybridge: solo un comando dead se reintenta (está %s)", c.Status)
	}
	now := o.cfg.Now().UTC()
	c.Status, c.Attempts, c.NextAttemptAt, c.CreatedAt, c.UpdatedAt = CommandPending, 0, now, now, now
	if err := o.cfg.Store.UpdateCommand(ctx, c); err != nil {
		return c, err
	}
	return c, o.kick(ctx, AggregateRef{Org: c.Org, System: c.System, AggregateKey: c.AggregateKey}, time.Time{})
}

// Discard descarta un comando muerto (decisión explícita de un operador) y
// deja seguir al resto de su agregado.
func (o *Outbound) Discard(ctx context.Context, id, actor string) (Command, error) {
	c, ok, err := o.cfg.Store.GetCommand(ctx, id)
	if err != nil {
		return c, err
	}
	if !ok {
		return c, ErrCommandNotFound
	}
	if c.Status != CommandDead {
		return c, fmt.Errorf("legacybridge: solo un comando dead se descarta (está %s)", c.Status)
	}
	c.Status, c.UpdatedAt = CommandDiscarded, o.cfg.Now().UTC()
	c.LastError = truncate(strings.TrimSpace("descartado por "+actor+": "+c.LastError), maxCommandError)
	if err := o.cfg.Store.UpdateCommand(ctx, c); err != nil {
		return c, err
	}
	return c, o.kick(ctx, AggregateRef{Org: c.Org, System: c.System, AggregateKey: c.AggregateKey}, time.Time{})
}

// ErrCommandNotFound: no hay comando con ese id.
var ErrCommandNotFound = errors.New("legacybridge: comando no encontrado")

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// --- transacción en el contexto ------------------------------------------------

type txKey struct{}

// WithTx guarda tx en el contexto: GormStore, Outbound.Publish y el Sink del
// host la usan en lugar de su conexión.
func WithTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFrom devuelve la transacción del contexto, si hay.
func TxFrom(ctx context.Context) (*gorm.DB, bool) {
	tx, ok := ctx.Value(txKey{}).(*gorm.DB)
	return tx, ok && tx != nil
}

// DB devuelve la transacción del contexto o, si no hay, fallback con ctx.
func DB(ctx context.Context, fallback *gorm.DB) *gorm.DB {
	if tx, ok := TxFrom(ctx); ok {
		return tx
	}
	return fallback.WithContext(ctx)
}

// GormAtomic es un Config.Atomic sobre db: abre una transacción (o reusa la del
// contexto) y la pasa a fn por el contexto.
func GormAtomic(db *gorm.DB) func(ctx context.Context, fn func(ctx context.Context) error) error {
	return func(ctx context.Context, fn func(context.Context) error) error {
		if _, ok := TxFrom(ctx); ok {
			return fn(ctx)
		}
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return fn(WithTx(ctx, tx))
		})
	}
}
