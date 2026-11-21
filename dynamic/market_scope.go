package dynamic

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/gorm"
)

// Market scope — contrato de región del kernel.
//
// Una organización con varios mercados (mx, us…) ata cada sucursal a un mercado; la sucursal activa de la sesión fija entonces el
// mercado de la llamada. Un modelo que declara la columna `markets` (jsonb: lista de códigos; vacío = todos) solo muestra sus filas
// en los mercados listados. El kernel no sabe qué es un mercado en términos de negocio: el host resuelve el código y lo estampa con
// WithMarket (ya existente); MarketScoper (o ApplyMarketScope en consultas escritas a mano) aplica la regla igual en listas, agregados, opciones,
// búsqueda, facetas y tablero, de modo que nadie filtra por mercado por su cuenta.
//
// Reglas (no cambian por host):
//   - sin mercado en la llamada: sin filtro (nada cambia para quien no usa mercados);
//   - tabla sin columna `markets` jsonb: sin filtro;
//   - fila con markets NULL o '[]': visible en todos los mercados;
//   - fila con códigos: visible solo si el mercado de la llamada está en la lista.

// MarketsColumn es la columna convencional que declara los mercados de una fila.
const MarketsColumn = "markets"

// NormalizeMarket deja el código en su forma canónica (minúsculas, sin espacios). El portador del mercado en el contexto ya existe
// (WithMarket / MarketFromContext, canonical_events.go; lo estampan los hosts desde el principal); aquí solo se consume.
func NormalizeMarket(m string) string { return strings.ToLower(strings.TrimSpace(m)) }

// ApplyMarketScope estrecha db a las filas visibles en `market` para table. No-op sin mercado o si la tabla no tiene `markets` jsonb.
func ApplyMarketScope(db *gorm.DB, table, market string) *gorm.DB {
	market = NormalizeMarket(market)
	if market == "" || table == "" || ColumnDataType(db, table, MarketsColumn) != "jsonb" {
		return db
	}
	return ApplyMarketPredicate(db, table, market)
}

// ApplyMarketPredicate es el predicado puro (sin mirar el esquema). jsonb_exists en vez del operador ? (gorm usa ? como parámetro).
func ApplyMarketPredicate(db *gorm.DB, table, market string) *gorm.DB {
	col := quoteIdent(MarketsColumn)
	if table != "" {
		parts := strings.Split(strings.ReplaceAll(table, `"`, ""), ".")
		for i, p := range parts {
			parts[i] = quoteIdent(p)
		}
		col = strings.Join(parts, ".") + "." + col
	}
	return db.Where("("+col+" IS NULL OR "+col+" = '[]'::jsonb OR jsonb_exists("+col+", ?))", market)
}

// ApplyMarketScopeFromContext aplica el mercado de ctx (si lo hay) a una consulta sobre table que el scoper no arma.
func ApplyMarketScopeFromContext(ctx context.Context, db *gorm.DB, table string) *gorm.DB {
	return ApplyMarketScope(db, table, MarketFromContext(ctx))
}

// MarketScoper envuelve cualquier TenantScoper y le añade el mercado de la llamada. Es composable: un host con alcance por sucursal
// envuelve su scoper de sucursal y obtiene las dos dimensiones.
type MarketScoper struct{ Inner TenantScoper }

func (s MarketScoper) inner() TenantScoper {
	if s.Inner == nil {
		return OrganizationScoper{}
	}
	return s.Inner
}

func (s MarketScoper) ScopeQuery(db *gorm.DB, user modelbase.AuthUser) *gorm.DB {
	db = s.inner().ScopeQuery(db, user)
	if db.Statement != nil && db.Statement.Context != nil {
		if m := MarketFromContext(db.Statement.Context); m != "" {
			db = ApplyMarketScope(db, db.Statement.Table, m)
		}
	}
	return db
}

func (s MarketScoper) InjectOnCreate(input map[string]any, user modelbase.AuthUser) {
	s.inner().InjectOnCreate(input, user)
}

// ColumnDataType devuelve el data_type de table.column ("" si no existe), cacheado un minuto por pool (los addons agregan columnas
// al instalarse). Dentro de una transacción no se cachea: puede ver DDL aún sin confirmar.
func ColumnDataType(db *gorm.DB, table, column string) string {
	if db == nil || db.Statement == nil {
		return ""
	}
	pool := db.Statement.ConnPool
	key := colTypeKey{pool: pool, table: table, column: column}
	_, isTx := pool.(gorm.TxCommitter)
	if !isTx {
		if v, ok := colTypeCache.Load(key); ok {
			if cv := v.(colTypeVal); time.Since(cv.at) < time.Minute {
				return cv.typ
			}
		}
	}
	schema, name := "", strings.ReplaceAll(table, `"`, "")
	if i := strings.Index(name, "."); i > 0 {
		schema, name = name[:i], name[i+1:]
	}
	var typ string
	q := db.Session(&gorm.Session{NewDB: true}).WithContext(db.Statement.Context)
	if schema != "" {
		q.Raw("SELECT data_type FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = ? LIMIT 1", schema, name, column).Scan(&typ)
	} else {
		q.Raw("SELECT data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ? LIMIT 1", name, column).Scan(&typ)
	}
	typ = strings.ToLower(strings.TrimSpace(typ))
	if !isTx {
		colTypeCache.Store(key, colTypeVal{typ: typ, at: time.Now()})
	}
	return typ
}

type colTypeKey struct {
	pool          gorm.ConnPool
	table, column string
}

type colTypeVal struct {
	typ string
	at  time.Time
}

var colTypeCache sync.Map
