package legacybridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/dynamic"
)

// CorrelationPrefix marca las escrituras que hace el propio puente (la réplica).
// Router ignora los eventos con ese prefijo en CorrelationID para no
// devolverle al legado lo que el legado mandó.
const CorrelationPrefix = "legacybridge:"

// Direction dice cuándo una ruta produce comandos.
type Direction string

const (
	// ToOwner: el usuario actuó en el host sobre algo cuyo dueño es el legado
	// (alta de cliente, abono a una factura del legado): comando hacia el dueño.
	ToOwner Direction = "to_owner"
	// Replicate: el host es dueño y el legado recibe la réplica (existencias de
	// una sucursal ya cortada, maestros en F3).
	Replicate Direction = "replicate"
)

// Translate convierte un evento en comandos. Org, System, IdempotencyKey y
// AggregateKey vacíos los completa Router.
type Translate func(ctx context.Context, ev dynamic.CanonicalEvent) ([]Command, error)

// Route es una traducción de eventos de un modelo del host.
type Route struct {
	// Model es CanonicalEvent.Model.
	Model string
	// Actions filtra (created|updated|deleted). Vacío = todas.
	Actions []string
	// Entity es la entidad en el mapa de propiedad (vacío = Model).
	Entity    string
	Direction Direction
	Translate Translate
}

// RouterConfig arma un Router.
type RouterConfig struct {
	System       string
	Store        OwnershipStore
	Outbound     *Outbound
	DefaultOwner Owner
	Routes       []Route
}

// Router traduce eventos del host en comandos hacia el legado según el mapa de
// propiedad.
type Router struct {
	cfg    RouterConfig
	routes map[string][]Route
}

// NewRouter valida la configuración.
func NewRouter(cfg RouterConfig) (*Router, error) {
	if cfg.System == "" || cfg.Store == nil || cfg.Outbound == nil {
		return nil, errors.New("legacybridge: NewRouter requiere System, Store y Outbound")
	}
	if cfg.DefaultOwner == "" {
		cfg.DefaultOwner = OwnerLegacy
	}
	r := &Router{cfg: cfg, routes: map[string][]Route{}}
	for _, rt := range cfg.Routes {
		if rt.Model == "" || rt.Translate == nil {
			return nil, errors.New("legacybridge: ruta sin Model o Translate")
		}
		if rt.Direction != ToOwner && rt.Direction != Replicate {
			return nil, fmt.Errorf("legacybridge: ruta %s con dirección %q", rt.Model, rt.Direction)
		}
		r.routes[rt.Model] = append(r.routes[rt.Model], rt)
	}
	return r, nil
}

// Models lista los modelos con rutas (para suscribirse solo a ellos).
func (r *Router) Models() []string {
	out := make([]string, 0, len(r.routes))
	for m := range r.routes {
		out = append(out, m)
	}
	return out
}

// Handle publica los comandos que produce ev. Si ctx trae una transacción,
// los comandos quedan en ella.
func (r *Router) Handle(ctx context.Context, org string, ev dynamic.CanonicalEvent) ([]Command, error) {
	if strings.HasPrefix(ev.CorrelationID, CorrelationPrefix) {
		return nil, nil
	}
	routes := r.routes[ev.Model]
	if len(routes) == 0 {
		return nil, nil
	}
	rules, err := r.cfg.Store.Rules(ctx, org, r.cfg.System)
	if err != nil {
		return nil, err
	}
	phase, err := r.cfg.Store.Phase(ctx, org, r.cfg.System)
	if err != nil {
		return nil, err
	}
	occurrence := ev.OccurrenceID
	if occurrence == "" {
		occurrence = ev.ID
	}
	var out []Command
	for _, rt := range routes {
		if len(rt.Actions) > 0 && !contains(rt.Actions, ev.Action) {
			continue
		}
		entity := rt.Entity
		if entity == "" {
			entity = rt.Model
		}
		owner := ResolveOwner(rules, entity, ev.BranchID, phase, r.cfg.DefaultOwner)
		if (rt.Direction == ToOwner && owner != OwnerLegacy) || (rt.Direction == Replicate && owner != OwnerLocal) {
			continue
		}
		cmds, err := rt.Translate(ctx, ev)
		if err != nil {
			return out, fmt.Errorf("legacybridge: traducir %s.%s: %w", ev.Model, ev.Action, err)
		}
		for i, c := range cmds {
			c.Org, c.System = org, r.cfg.System
			if c.IdempotencyKey == "" {
				c.IdempotencyKey = fmt.Sprintf("%s:%s:%d", occurrence, c.Kind, i)
			}
			if c.AggregateKey == "" {
				c.AggregateKey = entity + ":" + rowID(ev)
			}
			saved, err := r.cfg.Outbound.Publish(ctx, c)
			if err != nil {
				return out, err
			}
			out = append(out, saved)
		}
	}
	return out, nil
}

func rowID(ev dynamic.CanonicalEvent) string {
	for _, m := range []map[string]any{ev.After, ev.Before} {
		if v, ok := m["id"]; ok && v != nil {
			return fmt.Sprint(v)
		}
	}
	return ev.ID
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
