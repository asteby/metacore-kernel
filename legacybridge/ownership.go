package legacybridge

import (
	"fmt"
	"strings"
	"time"
)

// Owner es el sistema de registro de un alcance.
type Owner string

const (
	// OwnerLegacy: el legado es dueño; el host tiene una réplica de solo lectura.
	OwnerLegacy Owner = "legacy"
	// OwnerLocal: el host es dueño; el legado recibe réplica o nada.
	OwnerLocal Owner = "local"
)

// Valid reporta si o es un dueño conocido.
func (o Owner) Valid() bool { return o == OwnerLegacy || o == OwnerLocal }

// Wildcard en una regla vale para cualquier entidad, sucursal o fase.
const Wildcard = "*"

// OwnershipRule asigna dueño a (entidad, sucursal, fase) de un sistema legado.
// Entity, Branch y Phase vacíos o "*" son comodín. Branch es el id LOCAL de la
// sucursal (el que lleva un CanonicalEvent); el aplicador traduce la sucursal
// del envelope por bridge_refs.
type OwnershipRule struct {
	Org    string `json:"org"`
	System string `json:"system"`
	Entity string `json:"entity"`
	Branch string `json:"branch"`
	Phase  string `json:"phase"`
	Owner  Owner  `json:"owner"`
	// Note y UpdatedBy dejan bitácora del cambio (p. ej. el flip de una sucursal).
	Note      string    `json:"note,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func isWild(s string) bool { s = strings.TrimSpace(s); return s == "" || s == Wildcard }

func normWild(s string) string {
	if isWild(s) {
		return Wildcard
	}
	return strings.TrimSpace(s)
}

// Normalize deja los comodines como "*" y valida el dueño.
func (r OwnershipRule) Normalize() (OwnershipRule, error) {
	r.Entity, r.Branch, r.Phase = normWild(r.Entity), normWild(r.Branch), normWild(r.Phase)
	if r.System == "" {
		return r, fmt.Errorf("legacybridge: regla de propiedad sin system")
	}
	if !r.Owner.Valid() {
		return r, fmt.Errorf("legacybridge: dueño %q inválido (legacy|local)", r.Owner)
	}
	return r, nil
}

// specificity: la entidad pesa más que la sucursal y la sucursal más que la fase.
func (r OwnershipRule) specificity() int {
	n := 0
	if !isWild(r.Entity) {
		n += 4
	}
	if !isWild(r.Branch) {
		n += 2
	}
	if !isWild(r.Phase) {
		n++
	}
	return n
}

func (r OwnershipRule) matches(entity, branch, phase string) bool {
	return (isWild(r.Entity) || r.Entity == entity) &&
		(isWild(r.Branch) || r.Branch == branch) &&
		(isWild(r.Phase) || r.Phase == phase)
}

// ResolveOwner elige el dueño de (entidad, sucursal) en la fase dada: gana la
// regla que coincide y es más específica; ante un empate de especificidad gana
// local sobre legacy (el lado que ya cortó no vuelve a recibir escrituras del
// otro). Sin regla, devuelve def.
func ResolveOwner(rules []OwnershipRule, entity, branch, phase string, def Owner) Owner {
	best, bestSpec := def, -1
	for _, r := range rules {
		if !r.matches(entity, branch, phase) {
			continue
		}
		s := r.specificity()
		if s > bestSpec || (s == bestSpec && r.Owner == OwnerLocal) {
			best, bestSpec = r.Owner, s
		}
	}
	return best
}
