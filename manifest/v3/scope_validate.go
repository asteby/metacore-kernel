package v3

import "fmt"

// Model.scope — política de alcance declarada por modelo (RFC 0006, contrato de región y de comercio).
//
// Un manifiesto dice QUÉ ES cada modelo respecto de las fronteras de una organización con varios países/comercios, en vez de que el
// host lo adivine por convención (hoy: «tiene una columna markets jsonb»):
//
//	merchant  pertenece a un comercio (por su sucursal): solo visible en él.            pedidos, cobros, facturas, movimientos
//	market    maestro compartido, visible según su columna `markets`.                    productos, reglas de descuento
//	shared    maestro global de la organización, visible desde cualquier comercio.       clientes, categorías, marcas
//	group     solo para la vista de grupo (consolidados, configuración de comercios).
//
// Vacío = sin declarar (compatibilidad: nada cambia). El kernel solo valida la coherencia; QUIEN la hace cumplir es el host
// (Ops: DataScope) y su prueba de aislamiento.

// ScopeKind es el valor de Model.scope.
type ScopeKind string

const (
	ScopeMerchant ScopeKind = "merchant"
	ScopeMarket   ScopeKind = "market"
	ScopeShared   ScopeKind = "shared"
	ScopeGroup    ScopeKind = "group"
)

// ScopeKinds son los valores declarables, en el orden en que se documentan.
var ScopeKinds = []ScopeKind{ScopeMerchant, ScopeMarket, ScopeShared, ScopeGroup}

// Valid reporta si k es un valor declarable (el vacío no lo es: «sin declarar» se representa con "").
func (k ScopeKind) Valid() bool {
	for _, v := range ScopeKinds {
		if k == v {
			return true
		}
	}
	return false
}

// marketsColumnName es la columna convencional que el host usa para filtrar por mercado (dynamic.MarketsColumn).
const marketsColumnName = "markets"

// validateScope comprueba la coherencia de Model.scope: valor conocido y, para `market`, la columna `markets` jsonb que el filtro
// necesita (sin ella el modelo declararía un alcance que nadie puede hacer cumplir).
func validateScope(m *Manifest) []string {
	var errs []string
	for mi, mod := range m.Models {
		if mod.Scope == "" {
			continue
		}
		where := fmt.Sprintf("models[%d] (%s).scope", mi, mod.Key)
		if !mod.Scope.Valid() {
			errs = append(errs, fmt.Sprintf("%s %q must be one of merchant, market, shared, group", where, mod.Scope))
			continue
		}
		if mod.Scope != ScopeMarket {
			continue
		}
		var col *Column
		for ci := range mod.Columns {
			if mod.Columns[ci].Name == marketsColumnName {
				col = &mod.Columns[ci]
				break
			}
		}
		if col == nil {
			errs = append(errs, fmt.Sprintf("%s \"market\" needs a %q column (jsonb: list of market codes, empty = every market)", where, marketsColumnName))
		} else if col.Type != "jsonb" {
			errs = append(errs, fmt.Sprintf("%s \"market\": column %q must be jsonb, got %q", where, marketsColumnName, col.Type))
		}
	}
	return errs
}
