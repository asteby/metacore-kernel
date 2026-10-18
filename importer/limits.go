package importer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ParseLimits acota lo que un parser acepta ANTES de armar las filas en
// memoria. Un archivo pequeño puede pedir mucha memoria si nadie mira su forma
// (p. ej. un CSV con un encabezado de 200 000 columnas y filas de una celda, o
// un JSON `[{},{},...]`): los topes se revisan mientras se lee, no después.
// Cero en un campo = sin tope en ese campo.
type ParseLimits struct {
	// MaxRows es el máximo de filas de datos (sin contar el encabezado).
	MaxRows int
	// MaxColumns es el máximo de columnas, contando el ENCABEZADO de un CSV y las
	// llaves de cada objeto de un JSON.
	MaxColumns int
	// MaxCellBytes es el máximo de bytes de una celda.
	MaxCellBytes int
}

// ErrParseLimit dice qué tope se pasó y dónde (fila 1-based de datos; 0 = encabezado).
type ErrParseLimit struct {
	What  string // "rows", "columns" o "cell_bytes"
	Limit int
	Row   int
}

func (e ErrParseLimit) Error() string {
	switch e.What {
	case "rows":
		return fmt.Sprintf("demasiadas filas; máximo permitido: %d", e.Limit)
	case "columns":
		if e.Row == 0 {
			return fmt.Sprintf("el encabezado tiene demasiadas columnas; máximo permitido: %d", e.Limit)
		}
		return fmt.Sprintf("la fila %d tiene demasiadas columnas; máximo permitido: %d", e.Row, e.Limit)
	default:
		return fmt.Sprintf("la fila %d tiene una celda de más de %d bytes", e.Row, e.Limit)
	}
}

// CSVReadError es un fallo al leer o decodificar el CSV (no un tope ni un
// error devuelto por fn).
type CSVReadError struct{ Err error }

func (e CSVReadError) Error() string { return "csv decode: " + e.Err.Error() }
func (e CSVReadError) Unwrap() error { return e.Err }

// EachCSV lee un CSV fila por fila (sin ReadAll), con la primera fila como
// encabezado, y llama fn con cada fila de datos. Corta en cuanto se pasa un
// tope. Cada mapa se reserva con el tamaño de lo que trae la fila, no del
// encabezado.
func EachCSV(r io.Reader, lim ParseLimits, fn func(row map[string]any) error) error {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // filas irregulares se toleran
	var headers []string
	n := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return CSVReadError{Err: err}
		}
		if headers == nil {
			if lim.MaxColumns > 0 && len(rec) > lim.MaxColumns {
				return ErrParseLimit{What: "columns", Limit: lim.MaxColumns}
			}
			headers = append([]string(nil), rec...)
			if len(headers) > 0 {
				headers[0] = StripBOM(headers[0])
			}
			continue
		}
		n++
		if lim.MaxRows > 0 && n > lim.MaxRows {
			return ErrParseLimit{What: "rows", Limit: lim.MaxRows, Row: n}
		}
		if lim.MaxColumns > 0 && len(rec) > lim.MaxColumns {
			return ErrParseLimit{What: "columns", Limit: lim.MaxColumns, Row: n}
		}
		cells := min(len(rec), len(headers))
		row := make(map[string]any, cells)
		for i := 0; i < cells; i++ {
			if lim.MaxCellBytes > 0 && len(rec[i]) > lim.MaxCellBytes {
				return ErrParseLimit{What: "cell_bytes", Limit: lim.MaxCellBytes, Row: n}
			}
			row[headers[i]] = rec[i]
		}
		if err := fn(row); err != nil {
			return err
		}
	}
}

// ParseCSVLimited es ParseCSV con topes revisados mientras se lee.
func ParseCSVLimited(r io.Reader, lim ParseLimits) ([]map[string]any, error) {
	var rows []map[string]any
	err := EachCSV(r, lim, func(row map[string]any) error {
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ParseJSONLimited es ParseJSON (arreglo de objetos o sobre {"data": [...]})
// con topes revisados objeto por objeto: no decodifica el arreglo completo antes
// de contar.
func ParseJSONLimited(body []byte, lim ParseLimits) ([]map[string]any, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	readArray := func() ([]map[string]any, error) {
		var rows []map[string]any
		for n := 1; dec.More(); n++ {
			if lim.MaxRows > 0 && n > lim.MaxRows {
				return nil, ErrParseLimit{What: "rows", Limit: lim.MaxRows, Row: n}
			}
			var row map[string]any
			if err := dec.Decode(&row); err != nil {
				return nil, err
			}
			if err := checkJSONRow(row, lim, n); err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		if _, err := dec.Token(); err != nil { // ']'
			return nil, err
		}
		return rows, nil
	}
	switch tok {
	case json.Delim('['):
		return readArray()
	case json.Delim('{'):
		var rows []map[string]any
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if k, _ := kt.(string); k == "data" {
				t, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if t != json.Delim('[') {
					return nil, fmt.Errorf("json: \"data\" debe ser un arreglo")
				}
				if rows, err = readArray(); err != nil {
					return nil, err
				}
				continue
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("json: se esperaba un arreglo de objetos o {\"data\": [...]}")
	}
}

func checkJSONRow(row map[string]any, lim ParseLimits, n int) error {
	if lim.MaxColumns > 0 && len(row) > lim.MaxColumns {
		return ErrParseLimit{What: "columns", Limit: lim.MaxColumns, Row: n}
	}
	if lim.MaxCellBytes > 0 {
		for _, v := range row {
			if s, ok := v.(string); ok && len(s) > lim.MaxCellBytes {
				return ErrParseLimit{What: "cell_bytes", Limit: lim.MaxCellBytes, Row: n}
			}
		}
	}
	return nil
}
