package webhooks

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// SchemaSource declara, por evento, las rutas de `payload.*` que existen. Es lo
// que permite fallar AL GUARDAR (no al entregar) cuando una transformacion o un
// filtro nombra una ruta inexistente. El host lo alimenta desde los contratos
// de sus eventos (manifest, modelos dinamicos...).
type SchemaSource interface {
	// PayloadPaths devuelve las rutas del payload del evento (sin el prefijo
	// "payload."): "id", "customer.name", "items.*.sku". ok=false: evento
	// desconocido.
	PayloadPaths(event string) (paths []string, ok bool)
	// EventNames lista los eventos conocidos (para expandir patrones).
	EventNames() []string
}

// Schemas es un SchemaSource en memoria.
//
//	s := webhooks.NewSchemas()
//	s.Register("customers.customer.created", "id", "name", "email", "address.city", "tags.*")
type Schemas struct {
	mu sync.RWMutex
	m  map[string][]string
}

// NewSchemas crea un registro vacio.
func NewSchemas() *Schemas { return &Schemas{m: map[string][]string{}} }

// Register declara las rutas de payload de un evento (reemplaza lo anterior).
// Un segmento "*" cubre cualquier indice de arreglo.
func (s *Schemas) Register(event string, paths ...string) *Schemas {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[event] = append([]string(nil), paths...)
	return s
}

func (s *Schemas) PayloadPaths(event string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.m[event]
	return p, ok
}

func (s *Schemas) EventNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pathDeclared: p (segmentos tras "payload.") existe si es EXACTAMENTE una ruta
// declarada (un campo escalar, no un subarbol); un indice numerico casa con "*".
func pathDeclared(declared []string, segs []string) bool {
	if len(segs) == 0 {
		return true
	}
	for _, d := range declared {
		ds := strings.Split(d, ".")
		if len(segs) != len(ds) { // exact scalar field: never an object/array subtree
			continue
		}
		ok := true
		for i, s := range segs {
			if ds[i] == s {
				continue
			}
			if _, err := strconv.Atoi(s); err == nil && ds[i] == "*" {
				continue
			}
			ok = false
			break
		}
		if ok {
			return true
		}
	}
	return false
}

// checkRefs valida las rutas contra los eventos a los que aplica la suscripcion.
func checkRefs(src SchemaSource, events []string, refs []pathRef) error {
	var payloadRefs []pathRef
	for _, r := range refs {
		if r.root == "payload" && len(r.segs) > 0 {
			payloadRefs = append(payloadRefs, r)
		}
	}
	if len(payloadRefs) == 0 {
		return nil
	}
	if src == nil {
		return fmt.Errorf("%w: cannot validate %q without a SchemaSource", ErrNoSchema, payloadRefs[0].raw)
	}
	for _, pat := range events {
		var names []string
		if strings.Contains(pat, "*") {
			for _, n := range src.EventNames() {
				if containsEvent(StringSlice{pat}, n) {
					names = append(names, n)
				}
			}
			if len(names) == 0 {
				return fmt.Errorf("%w: pattern %q matches no known event", ErrNoSchema, pat)
			}
		} else {
			names = []string{pat}
		}
		for _, n := range names {
			paths, ok := src.PayloadPaths(n)
			if !ok {
				return fmt.Errorf("%w: event %q has no schema", ErrNoSchema, n)
			}
			for _, r := range payloadRefs {
				if !pathDeclared(paths, r.segs) {
					return fmt.Errorf("%w: %q does not exist in event %q", ErrUnknownPath, r.raw, n)
				}
			}
		}
	}
	return nil
}
