package sqlpoll

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	lb "github.com/asteby/metacore-kernel/legacybridge"
)

// Dialect es el motor de la base del legado.
type Dialect string

const (
	MySQL    Dialect = "mysql" // también MariaDB
	Postgres Dialect = "postgres"
	SQLite   Dialect = "sqlite"
)

func (d Dialect) valid() bool { return d == MySQL || d == Postgres || d == SQLite }

func (d Dialect) quote(ident string) string {
	if d == MySQL {
		return "`" + ident + "`"
	}
	return `"` + ident + `"`
}

// keyExpr es la llave del flujo como expresión de orden y comparación. Una
// llave de texto va en orden binario (bytes), el mismo que strings.Compare.
func (d Dialect) keyExpr(st Stream) string {
	k := d.quote(st.Key)
	if st.KeyType != KeyText {
		return k
	}
	switch d {
	case MySQL:
		return "BINARY " + k
	case Postgres:
		return k + ` COLLATE "C"`
	default:
		return k + " COLLATE BINARY"
	}
}

// placeholders numera los parámetros ($1… en Postgres, ? en los demás).
type placeholders struct {
	d Dialect
	n int
}

func (p *placeholders) next() string {
	p.n++
	if p.d == Postgres {
		return "$" + strconv.Itoa(p.n)
	}
	return "?"
}

// Límites de una consulta.
const (
	DefaultPageSize = 500
	MaxPageSize     = 5000
	// DefaultSettle es cuánto se espera a que una fila modificada se asiente
	// (transacciones abiertas en el legado) antes de leerla.
	DefaultSettle = 10 * time.Second
)

// Source lee una base del legado. DB debe abrirse con un usuario de SOLO
// LECTURA; en MySQL/MariaDB con parseTime=true&loc=UTC (las fechas se leen como
// reloj de pared y se interpretan en Location).
type Source struct {
	DB      *sql.DB
	Dialect Dialect
	Profile Profile
	// Location es la zona en la que el legado escribe sus fechas sin zona
	// (por omisión UTC).
	Location *time.Location
	// Settle: Poll no lee filas modificadas hace menos de esto (DefaultSettle).
	Settle   time.Duration
	PageSize int
	Now      func() time.Time
}

// New valida el perfil y completa los valores por omisión.
func New(db *sql.DB, d Dialect, p Profile) (*Source, error) {
	if db == nil {
		return nil, errors.New("sqlpoll: New requiere *sql.DB")
	}
	if !d.valid() {
		return nil, fmt.Errorf("sqlpoll: dialecto %q (mysql|postgres|sqlite)", d)
	}
	p, err := p.Normalize()
	if err != nil {
		return nil, err
	}
	return &Source{DB: db, Dialect: d, Profile: p}, nil
}

func (s *Source) loc() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.UTC
}

func (s *Source) pageSize() int {
	if s.PageSize <= 0 {
		return DefaultPageSize
	}
	return min(s.PageSize, MaxPageSize)
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) settle() time.Duration {
	if s.Settle < 0 {
		return 0
	}
	if s.Settle == 0 {
		return DefaultSettle
	}
	return s.Settle
}

// toWall pasa un instante al reloj de pared del legado (etiquetado UTC, como
// lo devuelve el driver con loc=UTC).
func (s *Source) toWall(t time.Time) time.Time {
	w := t.In(s.loc())
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
}

// toInstant interpreta un reloj de pared del legado en Location.
func (s *Source) toInstant(w time.Time) time.Time {
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), s.loc()).UTC()
}

// timeParam es el parámetro de fecha para el dialecto.
func (s *Source) timeParam(wall time.Time) any {
	switch s.Dialect {
	case Postgres:
		return wall
	case SQLite:
		return wall.Format("2006-01-02 15:04:05")
	default:
		return wall.Format("2006-01-02 15:04:05.000000")
	}
}

// --- cursor --------------------------------------------------------------------

const keyPad = 20

// encodeCursor: "<instante RFC3339Nano>#<llave>". Una llave entera va rellena
// de ceros y una de texto tal cual: en los dos casos lb.CompareCursors ordena
// igual que la consulta (Dialect.keyExpr).
func encodeCursor(st Stream, instant time.Time, key any) (string, error) {
	k, err := encodeKey(st, key)
	if err != nil {
		return "", err
	}
	return instant.UTC().Format(time.RFC3339Nano) + "#" + k, nil
}

func encodeKey(st Stream, key any) (string, error) {
	k := valueString(key)
	if st.KeyType == KeyText {
		return k, nil
	}
	n, err := strconv.ParseInt(k, 10, 64)
	if err != nil || n < 0 {
		return "", fmt.Errorf("sqlpoll: %s.%s = %q no es un entero no negativo (declara key_type \"text\")", st.Table, st.Key, k)
	}
	return fmt.Sprintf("%0*d", keyPad, n), nil
}

// decodeCursor devuelve el instante y la llave del cursor en el tipo de la
// llave del flujo.
func decodeCursor(st Stream, c string) (time.Time, any, bool, error) {
	if c == "" {
		return time.Time{}, nil, false, nil
	}
	ts, key, _ := strings.Cut(c, "#")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, nil, false, fmt.Errorf("sqlpoll: cursor %q ilegible: %w", c, err)
	}
	if st.KeyType == KeyText {
		return t, key, true, nil
	}
	n, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return time.Time{}, nil, false, fmt.Errorf("sqlpoll: cursor %q con llave no entera para %s", c, st.Name)
	}
	return t, n, true, nil
}

// ErrCursorStalled: la consulta devolvió un lote cuyo último cursor no es
// mayor que el de partida. Nunca debería pasar con keyExpr; si pasa (una
// collation o un tipo de llave mal declarado), el flujo se detiene con error
// en vez de releer el mismo lote para siempre.
var ErrCursorStalled = errors.New("sqlpoll: el cursor no avanza (el orden de la base no coincide con el del cursor; revisa key_type)")

// --- lectura -------------------------------------------------------------------

// row es una fila leída: columna → valor normalizado.
type row map[string]any

func (s *Source) query(ctx context.Context, q string, args ...any) ([]row, error) {
	rs, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []row
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := make(row, len(cols))
		for i, c := range cols {
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func (s *Source) selectList(st Stream) string {
	cols := st.selectColumns()
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = s.Dialect.quote(c)
	}
	return strings.Join(q, ", ")
}

// Poll lee hasta PageSize filas del flujo modificadas después de cursor, en
// orden (updated_at, llave), y devuelve el lote con el cursor de la última.
// Un lote sin envelopes trae el mismo cursor.
func (s *Source) Poll(ctx context.Context, stream, cursor string) (lb.Batch, error) {
	st, ok := s.Profile.Stream(stream)
	if !ok {
		return lb.Batch{}, fmt.Errorf("sqlpoll: flujo %q no está en el perfil", stream)
	}
	if st.UpdatedAt == "" {
		return lb.Batch{}, fmt.Errorf("sqlpoll: el flujo %q no tiene updated_at; se sincroniza con Reconcile", stream)
	}
	after, afterKey, hasCursor, err := decodeCursor(st, cursor)
	if err != nil {
		return lb.Batch{}, err
	}
	d, ph := s.Dialect, &placeholders{d: s.Dialect}
	upd, key := d.quote(st.UpdatedAt), d.keyExpr(st)
	bound := s.toWall(s.now().Add(-s.settle()))
	where := upd + " IS NOT NULL AND " + upd + " <= " + ph.next()
	args := []any{s.timeParam(bound)}
	if hasCursor {
		wall := s.timeParam(s.toWall(after))
		where += " AND (" + upd + " > " + ph.next() + " OR (" + upd + " = " + ph.next() + " AND " + key + " > " + ph.next() + "))"
		args = append(args, wall, wall, afterKey)
	}
	q := "SELECT " + s.selectList(st) + " FROM " + d.quote(st.Table) + " WHERE " + where +
		" ORDER BY " + upd + ", " + key + " LIMIT " + ph.next()
	args = append(args, s.pageSize())
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return lb.Batch{}, fmt.Errorf("sqlpoll: leer %s: %w", st.Table, err)
	}
	b := lb.Batch{Source: s.Profile.System, Stream: st.Name}
	for _, r := range rows {
		env, err := s.envelope(st, r)
		if err != nil {
			return lb.Batch{}, err
		}
		b.Envelopes = append(b.Envelopes, env)
	}
	if n := len(rows); n > 0 {
		last := rows[n-1]
		t, err := s.instantOf(last[st.UpdatedAt])
		if err != nil {
			return lb.Batch{}, err
		}
		if b.Cursor, err = encodeCursor(st, t, last[st.Key]); err != nil {
			return lb.Batch{}, err
		}
		if hasCursor && lb.CompareCursors(b.Cursor, cursor) <= 0 {
			return lb.Batch{}, fmt.Errorf("%w: %s de %s a %s", ErrCursorStalled, st.Name, cursor, b.Cursor)
		}
	} else {
		b.Cursor = cursor
	}
	return b, nil
}

// ExternalID es el id externo de una fila: "<system>:<tabla>:<llave>".
func (s *Source) ExternalID(table string, key any) string {
	return s.Profile.System + ":" + table + ":" + valueString(key)
}

// envelope arma el envelope de una fila.
func (s *Source) envelope(st Stream, r row) (lb.Envelope, error) {
	keyVal := r[st.Key]
	if keyVal == nil || valueString(keyVal) == "" {
		return lb.Envelope{}, fmt.Errorf("sqlpoll: %s trae una fila sin %s", st.Table, st.Key)
	}
	root := map[string]any{}
	var requires []lb.Dependency
	for _, c := range st.Columns {
		switch c.Kind {
		case KindConst:
			root[c.To] = c.Value
		case KindRef:
			v := s.normalize(r[c.From])
			ref := valueString(v)
			if v == nil || ref == "" || (c.Optional && ref == "0") {
				root[c.To] = nil
				continue
			}
			ext := s.ExternalID(c.RefTable, v)
			root[c.To] = ext
			requires = append(requires, lb.Dependency{Entity: c.RefEntity, ExternalID: ext})
		case KindBool:
			root[c.To] = truthy(s.normalize(r[c.From]))
		case KindMeta:
			m, _ := root[c.Into].(map[string]any)
			if m == nil {
				m = map[string]any{}
				root[c.Into] = m
			}
			m[c.To] = s.normalize(r[c.From])
		default:
			root[c.To] = s.normalize(r[c.From])
		}
	}
	env := lb.Envelope{Source: s.Profile.System, Entity: st.Entity, ExternalID: s.ExternalID(st.Table, keyVal), Op: lb.OpUpsert,
		Aggregate: lb.Aggregate{Root: root}, Requires: requires}
	if st.BranchColumn != "" {
		if b := valueString(s.normalize(r[st.BranchColumn])); b != "" && b != "0" {
			env.BranchExternalID = s.ExternalID(st.BranchTable, b)
		}
	}
	stamp := time.Time{}
	if st.UpdatedAt != "" && r[st.UpdatedAt] != nil {
		t, err := s.instantOf(r[st.UpdatedAt])
		if err != nil {
			return lb.Envelope{}, err
		}
		stamp = t
	}
	env.SourceVersion = stamp.UTC().Format(time.RFC3339Nano) + "#" + env.Hash()
	return env, nil
}

// normalize deja un valor del driver en un tipo JSON estable.
func (s *Source) normalize(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(x)
	case time.Time:
		return time.Date(x.Year(), x.Month(), x.Day(), x.Hour(), x.Minute(), x.Second(), x.Nanosecond(), s.loc()).Format(time.RFC3339Nano)
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float32:
		return float64(x)
	default:
		return x
	}
}

var wallLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02"}

// instantOf interpreta una fecha del legado (time.Time o texto) en Location.
func (s *Source) instantOf(v any) (time.Time, error) {
	switch x := v.(type) {
	case time.Time:
		return s.toInstant(x), nil
	case []byte:
		return s.instantOf(string(x))
	case string:
		for _, l := range wallLayouts {
			if t, err := time.Parse(l, x); err == nil {
				if l == time.RFC3339Nano {
					return t.UTC(), nil
				}
				return s.toInstant(t), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("sqlpoll: fecha ilegible %v (%T)", v, v)
}

func valueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "true", "t", "yes", "si", "sí", "y":
			return true
		}
	}
	return false
}
