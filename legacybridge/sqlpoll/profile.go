// Package sqlpoll es el adaptador de origen sql_poll del puente legado: lee
// por consulta una base del legado (MySQL/MariaDB, Postgres o SQLite) con un
// usuario de solo lectura y produce lotes de legacybridge.Envelope, uno por
// fila. Sirve para los maestros (catálogo, clientes, proveedores) de cualquier
// legado cuyas tablas tengan una llave y, de preferencia, una columna de
// última modificación.
//
// El mapeo es dato (Profile): qué tabla, qué llave, qué columna de cambios y
// cómo se copia cada columna. Las consultas las arma el paquete con
// identificadores validados y entre comillas del dialecto; los valores viajan
// siempre como parámetros. Nada del perfil se pega como SQL libre.
//
//   - Poll lee los cambios de un flujo después del cursor, en orden
//     (updated_at, llave), hasta un límite de asentamiento (Source.Settle) para
//     no adelantarse a transacciones todavía abiertas en el legado.
//   - Reconcile recorre la tabla completa por llave y la compara con
//     bridge_refs: reemite lo que falta o cambió sin tocar updated_at y emite
//     op=delete para las llaves que ya no existen (los borrados en duro).
//   - Sync corre Poll e Ingest en ciclo para cada flujo, con el cursor
//     guardado en el host.
package sqlpoll

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Tipos de columna.
const (
	// KindCopy copia el valor tal cual (por omisión).
	KindCopy = "copy"
	// KindRef convierte el valor en el external_id de otra tabla del legado
	// ("<system>:<ref_table>:<valor>") y lo declara dependencia del envelope.
	KindRef = "ref"
	// KindMeta guarda el valor dentro de un objeto JSON de la raíz (Into, por
	// omisión "metadata") con la llave To.
	KindMeta = "meta"
	// KindBool convierte 0/1, "0"/"1", "true"/"false" a booleano.
	KindBool = "bool"
	// KindConst pone Value en To; no lee columna (From vacío).
	KindConst = "const"
)

// Profile es el mapeo de un legado.
type Profile struct {
	// System es el prefijo de los external_id ("7l" → "7l:products:12").
	System  string   `json:"system"`
	Streams []Stream `json:"streams"`
}

// Stream es una tabla del legado que se replica como una entidad del host.
type Stream struct {
	// Name identifica el cursor (por omisión Table).
	Name  string `json:"name,omitempty"`
	Table string `json:"table"`
	// Entity es la entidad del host con la que el host guarda bridge_refs.
	Entity string `json:"entity"`
	// Key es la llave primaria de una sola columna (por omisión "id").
	Key string `json:"key,omitempty"`
	// UpdatedAt es la columna de última modificación. Vacía: el flujo solo se
	// sincroniza por Reconcile.
	UpdatedAt string `json:"updated_at,omitempty"`
	// BranchColumn (opcional) es la columna con la sucursal del registro;
	// BranchTable (por omisión "branches") arma su external_id.
	BranchColumn string   `json:"branch_column,omitempty"`
	BranchTable  string   `json:"branch_table,omitempty"`
	Columns      []Column `json:"columns"`
}

// Column copia una columna del legado a la raíz del envelope.
type Column struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	Kind string `json:"kind,omitempty"`
	// RefTable y RefEntity (KindRef): la tabla del legado referida y su entidad
	// en el host (la de la dependencia).
	RefTable  string `json:"ref_table,omitempty"`
	RefEntity string `json:"ref_entity,omitempty"`
	// Optional (KindRef): un valor vacío o 0 no es dependencia (FK opcional).
	Optional bool `json:"optional,omitempty"`
	// Into (KindMeta): el objeto JSON de la raíz (por omisión "metadata").
	Into string `json:"into,omitempty"`
	// Value (KindConst).
	Value any `json:"value,omitempty"`
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Límites del perfil.
const (
	MaxStreams = 64
	MaxColumns = 256
)

func checkIdent(what, s string) error {
	if !identRe.MatchString(s) {
		return fmt.Errorf("sqlpoll: %s %q no es un identificador válido ([A-Za-z_][A-Za-z0-9_]*, hasta 64)", what, s)
	}
	return nil
}

// Normalize valida el perfil y completa los valores por omisión. El perfil
// viene de configuración (no de una petición), pero igual se valida todo
// identificador antes de usarlo en una consulta.
func (p Profile) Normalize() (Profile, error) {
	p.System = strings.TrimSpace(p.System)
	if p.System == "" || strings.Contains(p.System, ":") || len(p.System) > 32 {
		return p, errors.New("sqlpoll: system vacío, con ':' o de más de 32 caracteres")
	}
	if len(p.Streams) == 0 || len(p.Streams) > MaxStreams {
		return p, fmt.Errorf("sqlpoll: el perfil debe traer entre 1 y %d flujos", MaxStreams)
	}
	names := map[string]bool{}
	out := make([]Stream, len(p.Streams))
	for i, s := range p.Streams {
		var err error
		if s, err = s.normalize(); err != nil {
			return p, fmt.Errorf("streams[%d]: %w", i, err)
		}
		if names[s.Name] {
			return p, fmt.Errorf("sqlpoll: flujo %q repetido", s.Name)
		}
		names[s.Name] = true
		out[i] = s
	}
	p.Streams = out
	return p, nil
}

func (s Stream) normalize() (Stream, error) {
	if s.Key == "" {
		s.Key = "id"
	}
	if s.Name == "" {
		s.Name = s.Table
	}
	if s.BranchTable == "" {
		s.BranchTable = "branches"
	}
	if strings.TrimSpace(s.Entity) == "" || len(s.Entity) > 128 {
		return s, errors.New("sqlpoll: entity vacía o de más de 128 caracteres")
	}
	if err := checkIdent("name", s.Name); err != nil {
		return s, err
	}
	for what, v := range map[string]string{"table": s.Table, "key": s.Key, "branch_table": s.BranchTable} {
		if err := checkIdent(what, v); err != nil {
			return s, err
		}
	}
	for what, v := range map[string]string{"updated_at": s.UpdatedAt, "branch_column": s.BranchColumn} {
		if v != "" {
			if err := checkIdent(what, v); err != nil {
				return s, err
			}
		}
	}
	if len(s.Columns) == 0 || len(s.Columns) > MaxColumns {
		return s, fmt.Errorf("sqlpoll: %s debe mapear entre 1 y %d columnas", s.Table, MaxColumns)
	}
	cols := make([]Column, len(s.Columns))
	for i, c := range s.Columns {
		if c.Kind == "" {
			c.Kind = KindCopy
		}
		if c.Kind == KindMeta && c.Into == "" {
			c.Into = "metadata"
		}
		if strings.TrimSpace(c.To) == "" || len(c.To) > 64 {
			return s, fmt.Errorf("sqlpoll: %s.columns[%d] sin to", s.Table, i)
		}
		switch c.Kind {
		case KindCopy, KindBool, KindMeta:
			if err := checkIdent("from", c.From); err != nil {
				return s, err
			}
		case KindRef:
			if err := checkIdent("from", c.From); err != nil {
				return s, err
			}
			if err := checkIdent("ref_table", c.RefTable); err != nil {
				return s, err
			}
			if strings.TrimSpace(c.RefEntity) == "" {
				return s, fmt.Errorf("sqlpoll: %s.%s es ref sin ref_entity", s.Table, c.From)
			}
		case KindConst:
			if c.From != "" {
				return s, fmt.Errorf("sqlpoll: %s.columns[%d] const no lee columna", s.Table, i)
			}
		default:
			return s, fmt.Errorf("sqlpoll: %s.columns[%d] kind %q desconocido", s.Table, i, c.Kind)
		}
		cols[i] = c
	}
	s.Columns = cols
	return s, nil
}

// Stream busca un flujo por nombre.
func (p Profile) Stream(name string) (Stream, bool) {
	for _, s := range p.Streams {
		if s.Name == name {
			return s, true
		}
	}
	return Stream{}, false
}

// selectColumns es la llave, la columna de cambios, la sucursal y cada From,
// sin repetir y en orden estable.
func (s Stream) selectColumns() []string {
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	add(s.Key)
	add(s.UpdatedAt)
	add(s.BranchColumn)
	for _, c := range s.Columns {
		add(c.From)
	}
	return out
}
