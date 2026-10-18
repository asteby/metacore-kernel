package dataload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
)

// Source entrega las filas crudas de una fuente. Los adaptadores son pequeños y
// no conocen el perfil: solo saben leer un formato.
type Source interface {
	// Name describe la fuente para el historial (nunca debe contener secretos).
	Name() string
	// Rows lee todas las filas, como mapas encabezado -> valor.
	Rows(ctx context.Context) ([]map[string]any, error)
}

// BytesSource es una fuente en memoria con un lector de formato.
type bytesSource struct {
	name  string
	body  []byte
	parse func([]byte) ([]map[string]any, error)
}

func (s bytesSource) Name() string { return s.name }
func (s bytesSource) Rows(ctx context.Context) ([]map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.parse(s.body)
}

// NewCSVSource lee un CSV (con o sin BOM; delimitador coma).
func NewCSVSource(name string, body []byte) Source {
	return bytesSource{name, body, func(b []byte) ([]map[string]any, error) {
		return importer.ParseCSV(bytes.NewReader(b))
	}}
}

// NewCSVReaderSource lee todo r y arma un CSVSource.
func NewCSVReaderSource(name string, r io.Reader) (Source, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return NewCSVSource(name, b), nil
}

// NewJSONSource lee un arreglo de objetos, o {"data":[...]}.
func NewJSONSource(name string, body []byte) Source {
	return bytesSource{name, body, importer.ParseJSON}
}

// NewBundleSource lee un objeto JSON y toma las filas de la ruta con puntos
// indicada ("products", "catalog.items"). Es el adaptador para bundles
// ya normalizados por otra herramienta. Un valor que no sea arreglo de objetos
// es error: no se adivina.
func NewBundleSource(name string, body []byte, path string) Source {
	return bytesSource{name, body, func(b []byte) ([]map[string]any, error) {
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("bundle inválido: %w", err)
		}
		cur := doc
		if path != "" {
			for _, seg := range strings.Split(path, ".") {
				m, ok := cur.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("bundle: %q no es un objeto en la ruta %q", seg, path)
				}
				if cur, ok = m[seg]; !ok {
					return nil, fmt.Errorf("bundle: no existe %q en la ruta %q", seg, path)
				}
			}
		}
		arr, ok := cur.([]any)
		if !ok {
			return nil, fmt.Errorf("bundle: la ruta %q no es un arreglo", path)
		}
		rows := make([]map[string]any, 0, len(arr))
		for i, it := range arr {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("bundle: el elemento %d no es un objeto", i)
			}
			rows = append(rows, m)
		}
		return rows, nil
	}}
}

// inputHash es la huella del contenido leído: independiente del formato y del
// orden de las llaves (json.Marshal ordena los mapas). El orden de las filas sí
// cuenta.
func inputHash(rows []map[string]any) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, r := range rows {
		_ = enc.Encode(r)
	}
	return hex.EncodeToString(h.Sum(nil))
}
