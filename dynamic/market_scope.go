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
//
// Un alcance de lectura autorizado y explícito reemplaza el filtro anterior por
// la unión de sus mercados. Un conjunto explícito vacío deniega filas de tablas
// con alcance de mercado. El mercado operativo permanece en WithMarket.

// MarketsColumn es la columna convencional que declara los mercados de una fila.
const MarketsColumn = "markets"

// NormalizeMarket deja el código en su forma canónica (minúsculas, sin espacios). El portador del mercado en el contexto ya existe
// (WithMarket / MarketFromContext, canonical_events.go; lo estampan los hosts desde el principal); aquí solo se consume.
func NormalizeMarket(m string) string { return strings.ToLower(strings.TrimSpace(m)) }

type marketAccessKey struct{}

// WithMarketAccess carries the markets already authorized by the host for a
// catalog query. An empty set denies access to market-scoped rows. This scope
// is independent of WithMarket, which carries a single operational market.
func WithMarketAccess(ctx context.Context, markets []string) context.Context {
	return context.WithValue(ctx, marketAccessKey{}, normalizeMarkets(markets))
}

// MarketAccessFromContext returns a copy of the authorized markets. A false
// result indicates that the host did not set a catalog scope.
func MarketAccessFromContext(ctx context.Context) ([]string, bool) {
	markets, ok := ctx.Value(marketAccessKey{}).([]string)
	return append([]string(nil), markets...), ok
}

func normalizeMarkets(markets []string) []string {
	normalized := make([]string, 0, len(markets))
	seen := make(map[string]bool, len(markets))
	for _, market := range markets {
		market = NormalizeMarket(market)
		if market == "" || seen[market] {
			continue
		}
		seen[market] = true
		normalized = append(normalized, market)
	}
	return normalized
}

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
	return applyMarketAccessPredicate(db, table, []string{market})
}

// ApplyMarketAccessScope restricts a market-scoped table to shared rows or
// rows visible in at least one authorized market. An empty set denies all
// rows in a market-scoped table.
func ApplyMarketAccessScope(db *gorm.DB, table string, markets []string) *gorm.DB {
	if table == "" {
		return db
	}
	typ, err := columnDataType(db, table, MarketsColumn)
	if err != nil {
		failed := db.Session(&gorm.Session{})
		failed.AddError(err)
		return failed
	}
	if typ != "jsonb" {
		return db
	}
	return applyMarketAccessPredicate(db, table, normalizeMarkets(markets))
}

func applyMarketAccessPredicate(db *gorm.DB, table string, markets []string) *gorm.DB {
	if len(markets) == 0 {
		return db.Where("1 = 0")
	}
	parts := strings.Split(strings.ReplaceAll(table, `"`, ""), ".")
	col := quoteIdent(MarketsColumn)
	if table != "" {
		for i, part := range parts {
			parts[i] = quoteIdent(part)
		}
		col = strings.Join(parts, ".") + "." + col
	}
	shared := "(" + col + " IS NULL OR " + col + " = '[]'::jsonb OR "
	if len(markets) == 1 {
		return db.Where(shared+"jsonb_exists("+col+", ?))", markets[0])
	}
	args := make([]any, len(markets))
	for i, market := range markets {
		args[i] = market
	}
	params := strings.Repeat("?, ", len(markets)-1) + "?"
	return db.Where(shared+"jsonb_exists_any("+col+", ARRAY["+params+"]::text[]))", args...)
}

// ApplyMarketScopeFromContext aplica el alcance de lectura explícito o, en su
// ausencia, el mercado operativo de ctx a una consulta que el scoper no arma.
func ApplyMarketScopeFromContext(ctx context.Context, db *gorm.DB, table string) *gorm.DB {
	if markets, ok := ctx.Value(marketAccessKey{}).([]string); ok {
		return ApplyMarketAccessScope(db, table, markets)
	}
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
		db = ApplyMarketScopeFromContext(db.Statement.Context, db, db.Statement.Table)
	}
	return db
}

func (s MarketScoper) InjectOnCreate(input map[string]any, user modelbase.AuthUser) {
	s.inner().InjectOnCreate(input, user)
}

// ColumnDataType devuelve el data_type de table.column ("" si no existe), cacheado un minuto por pool (los addons agregan columnas
// al instalarse). Dentro de una transacción no se cachea: puede ver DDL aún sin confirmar.
func ColumnDataType(db *gorm.DB, table, column string) string {
	typ, _ := columnDataType(db, table, column)
	return typ
}

func columnDataType(db *gorm.DB, table, column string) (string, error) {
	if db == nil || db.Statement == nil {
		return "", nil
	}
	pool := db.Statement.ConnPool
	key := colTypeKey{pool: pool, table: table, column: column}
	_, isTx := pool.(gorm.TxCommitter)
	if !isTx {
		if v, ok := colTypeCache.Load(key); ok {
			if cv := v.(colTypeVal); time.Since(cv.at) < time.Minute {
				return cv.typ, nil
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
		q = q.Raw("SELECT data_type FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = ? LIMIT 1", schema, name, column)
	} else {
		q = q.Raw("SELECT data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ? LIMIT 1", name, column)
	}
	if err := q.Scan(&typ).Error; err != nil {
		return "", err
	}
	typ = strings.ToLower(strings.TrimSpace(typ))
	if !isTx {
		colTypeCache.Store(key, colTypeVal{typ: typ, at: time.Now()})
	}
	return typ, nil
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
