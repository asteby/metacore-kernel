package guest

import (
	"encoding/json"
	"errors"
	"fmt"
)

// data_query (docs/wasm-abi.md § 15): lectura paginable y org-scoped de una
// tabla LÓGICA. Este archivo es puro (sin imports de host); el import vive en
// dataquery_wasm.go.

// dataQueryPageSize es el tope por página del host (dataQueryMaxLimit en
// runtime/wasm/dataquery_records.go: pide más y el host recorta a 200).
const dataQueryPageSize = 200

// Row es una fila de data_query: columna -> valor JSON (números float64).
type Row = map[string]any

// Q es la consulta de Query / QueryOptional.
type Q struct {
	// Where son filtros: escalar = igualdad (nil = IS NULL); un objeto
	// map[string]any{"gt": x} usa operadores (gt gte lt lte ne in). No admite
	// "id" (lo usa el cursor) ni organization_id / deleted_at (los pone el host).
	Where map[string]any
	// MaxRows es el tope explícito de filas. 0 = hasta agotar. Si hay más
	// filas que el tope, se devuelven MaxRows y el error es ErrTruncated: el
	// truncado nunca es silencioso.
	MaxRows int
}

// ErrTruncated: Query devolvió Q.MaxRows filas pero existían más.
var ErrTruncated = errors.New("guest: data_query result truncated at Q.MaxRows")

// DataQueryError es el error tipado de Query / QueryOptional.
type DataQueryError struct {
	// Code es el error.code del host ("forbidden", "invalid_request",
	// "db_error", "source_unavailable"...), o "decode" / "invalid_query".
	Code    string
	Message string
	// Absent != nil cuando el fallo es una ausencia de fuente (Query la
	// devuelve como error; QueryOptional la devuelve en Optional.Absent).
	Absent *Absence
}

// Error implementa error.
func (e *DataQueryError) Error() string {
	if e == nil {
		return "guest: <nil data_query error>"
	}
	if e.Absent != nil {
		return "data_query: " + e.Absent.Error()
	}
	if e.Message == "" {
		return "data_query: " + e.Code
	}
	return fmt.Sprintf("data_query: %s: %s", e.Code, e.Message)
}

// Unwrap expone la ausencia para errors.As(err, &absence).
func (e *DataQueryError) Unwrap() error {
	if e == nil || e.Absent == nil {
		return nil
	}
	return e.Absent
}

// Optional es el resultado tri-estado de QueryOptional: Absent != nil significa
// "fuente no disponible" (Rows vacío); Absent == nil, filas reales (quizá cero).
type Optional struct {
	Rows   []Row
	Absent *Absence
}

type dataQueryRequestWire struct {
	Table    string         `json:"table"`
	Where    map[string]any `json:"where,omitempty"`
	Limit    int            `json:"limit"`
	OrderBy  string         `json:"order_by"`
	OrderDir string         `json:"order_dir,omitempty"`
	// OnAbsent "empty": el host >= DG1.2 sirve vacío + meta.absent. Un host
	// 1.18 lo ignora (json.Unmarshal descarta claves desconocidas) y responde
	// con el error de siempre, que ClassifyHostError reconoce.
	OnAbsent string `json:"on_absent,omitempty"`
}

type dataQueryOKWire struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// decodeDataQueryPage clasifica y decodifica un sobre de data_query.
func decodeDataQueryPage(table string, envelope []byte) ([]Row, *Absence, error) {
	out := ClassifyHostError(envelope)
	if out.Absent != nil {
		if out.Absent.Source == "" {
			out.Absent.Source = table
		}
		return nil, out.Absent, nil
	}
	if out.Err != nil {
		var he *HostError
		if errors.As(out.Err, &he) {
			return nil, nil, &DataQueryError{Code: he.Code, Message: he.Message}
		}
		return nil, nil, &DataQueryError{Code: "decode", Message: out.Err.Error()}
	}
	var ok dataQueryOKWire
	if err := json.Unmarshal(envelope, &ok); err != nil {
		return nil, nil, &DataQueryError{Code: "decode", Message: err.Error()}
	}
	if len(ok.Data) == 0 || string(ok.Data) == "null" {
		return nil, nil, nil
	}
	// Forma real: data = {"rows":[...]}. Se acepta también data = [...].
	var obj struct {
		Rows []Row `json:"rows"`
	}
	if err := json.Unmarshal(ok.Data, &obj); err == nil {
		return obj.Rows, nil, nil
	}
	var arr []Row
	if err := json.Unmarshal(ok.Data, &arr); err != nil {
		return nil, nil, &DataQueryError{Code: "decode", Message: err.Error()}
	}
	return arr, nil, nil
}

// queryPages pagina por cursor (`order_by id` asc + `id > último`) usando
// `call`, que envía un request JSON al host y devuelve el sobre. Devuelve las
// filas, la ausencia (si la fuente no existe) o un error.
func queryPages(call func(req []byte) []byte, table string, q Q, onAbsent string) ([]Row, *Absence, error) {
	if table == "" {
		return nil, nil, &DataQueryError{Code: "invalid_query", Message: "table is required"}
	}
	if q.MaxRows < 0 {
		return nil, nil, &DataQueryError{Code: "invalid_query", Message: "MaxRows must be >= 0"}
	}
	if _, ok := q.Where["id"]; ok {
		return nil, nil, &DataQueryError{Code: "invalid_query", Message: "where.id is reserved for the pagination cursor"}
	}
	var rows []Row
	var cursor any
	for {
		limit := dataQueryPageSize
		if q.MaxRows > 0 {
			if rem := q.MaxRows - len(rows) + 1; rem < limit {
				limit = rem // +1: detecta que había más sin leer de sobra
			}
		}
		where := make(map[string]any, len(q.Where)+1)
		for k, v := range q.Where {
			where[k] = v
		}
		if cursor != nil {
			where["id"] = map[string]any{"gt": cursor}
		}
		req, err := json.Marshal(dataQueryRequestWire{
			Table: table, Where: where, Limit: limit, OrderBy: "id", OnAbsent: onAbsent,
		})
		if err != nil {
			return nil, nil, &DataQueryError{Code: "invalid_query", Message: err.Error()}
		}
		page, absent, err := decodeDataQueryPage(table, call(req))
		if err != nil {
			return nil, nil, err
		}
		if absent != nil {
			return nil, absent, nil
		}
		rows = append(rows, page...)
		if q.MaxRows > 0 && len(rows) > q.MaxRows {
			return rows[:q.MaxRows], nil, ErrTruncated
		}
		if len(page) < limit {
			return rows, nil, nil
		}
		last := page[len(page)-1]["id"]
		if last == nil {
			return nil, nil, &DataQueryError{Code: "invalid_query", Message: "row without id: cannot paginate"}
		}
		cursor = last
	}
}
