package legacybridge

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Op es la operación que trae un envelope.
type Op string

const (
	OpUpsert Op = "upsert"
	OpDelete Op = "delete"
)

// Límites de un lote.
const (
	// MaxEnvelopeBytes acota una línea NDJSON (un agregado completo).
	MaxEnvelopeBytes = 4 << 20
	// MaxBatchEnvelopes acota cuántos envelopes trae un lote.
	MaxBatchEnvelopes = 5000
)

// Aggregate es la raíz de un envelope y sus hijos completos. El aplicador
// REEMPLAZA el conjunto de hijos de cada clave: un hijo que ya no viene se
// borró en el origen.
type Aggregate struct {
	Root     map[string]any              `json:"root"`
	Children map[string][]map[string]any `json:"children,omitempty"`
}

// Facet es otra fila del host que deriva del mismo registro de origen (p. ej.
// la cuenta por cobrar de una venta a crédito). Comparte la versión del
// envelope y se mapea en bridge_refs con su propio external_id.
type Facet struct {
	Entity     string         `json:"entity"`
	ExternalID string         `json:"external_id"`
	Row        map[string]any `json:"row"`
}

// Dependency es un padre que debe existir en el host antes de aplicar el
// envelope (el producto de una línea, el cliente de una venta).
type Dependency struct {
	Entity     string `json:"entity"`
	ExternalID string `json:"external_id"`
}

// Envelope es un cambio de un agregado del legado.
type Envelope struct {
	// Source es el sistema de origen (el prefijo de sus external_id, p. ej. "erp").
	Source string `json:"source"`
	// Entity es la entidad del host ("<addon>.<Modelo>" o la tabla; la elige el perfil).
	Entity     string `json:"entity"`
	ExternalID string `json:"external_id"`
	Op         Op     `json:"op"`
	// SourceVersion es monótona por fila y determinista: "<RFC3339Nano>#<huella>".
	// Ver CompareVersions.
	SourceVersion string       `json:"source_version"`
	Aggregate     Aggregate    `json:"aggregate"`
	Facets        []Facet      `json:"facets,omitempty"`
	Requires      []Dependency `json:"requires,omitempty"`
	// BranchExternalID es la sucursal del registro en el origen. Vacío = alcance
	// de toda la organización.
	BranchExternalID string    `json:"branch_external_id,omitempty"`
	EmittedAt        time.Time `json:"emitted_at,omitempty"`
}

// Key identifica la fila de origen dentro del sistema.
func (e Envelope) Key() string { return e.Entity + "|" + e.ExternalID }

// Validate revisa la forma del envelope.
func (e Envelope) Validate() error {
	var errs []string
	if strings.TrimSpace(e.Source) == "" {
		errs = append(errs, "source vacío")
	}
	if strings.TrimSpace(e.Entity) == "" {
		errs = append(errs, "entity vacío")
	}
	if strings.TrimSpace(e.ExternalID) == "" {
		errs = append(errs, "external_id vacío")
	}
	if strings.TrimSpace(e.SourceVersion) == "" {
		errs = append(errs, "source_version vacío")
	}
	switch e.Op {
	case OpUpsert:
		if e.Aggregate.Root == nil {
			errs = append(errs, "upsert sin aggregate.root")
		}
	case OpDelete:
	default:
		errs = append(errs, fmt.Sprintf("op %q desconocida (upsert|delete)", e.Op))
	}
	for i, f := range e.Facets {
		if f.Entity == "" || f.ExternalID == "" {
			errs = append(errs, fmt.Sprintf("facets[%d] sin entity o external_id", i))
		}
	}
	for i, d := range e.Requires {
		if d.Entity == "" || d.ExternalID == "" {
			errs = append(errs, fmt.Sprintf("requires[%d] sin entity o external_id", i))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("legacybridge: envelope %s inválido: %s", e.Key(), strings.Join(errs, "; "))
	}
	return nil
}

// Hash es la huella del contenido (op, agregado y facetas), sin la versión ni
// la hora de emisión: dos emisiones del mismo dato dan la misma huella.
// encoding/json ordena las claves de los mapas, así que es determinista.
func (e Envelope) Hash() string {
	b, _ := json.Marshal(struct {
		Op        Op        `json:"op"`
		Aggregate Aggregate `json:"aggregate"`
		Facets    []Facet   `json:"facets,omitempty"`
		Branch    string    `json:"branch,omitempty"`
	}{e.Op, e.Aggregate, e.Facets, e.BranchExternalID})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Batch es lo que el adaptador de origen manda en una llamada.
type Batch struct {
	Source string `json:"source"`
	// Stream separa cursores dentro de un mismo origen (p. ej. una corrida por
	// sucursal). Vacío = "default".
	Stream string `json:"stream,omitempty"`
	// Cursor es la marca de agua del origen tras este lote. El host la guarda
	// solo si el lote se procesó sin errores de infraestructura.
	Cursor    string     `json:"cursor,omitempty"`
	Envelopes []Envelope `json:"envelopes"`
}

// ParseNDJSON lee un envelope por línea (las líneas vacías se ignoran).
func ParseNDJSON(r io.Reader) ([]Envelope, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxEnvelopeBytes)
	var out []Envelope
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		if len(out) >= MaxBatchEnvelopes {
			return nil, fmt.Errorf("legacybridge: el lote supera %d envelopes", MaxBatchEnvelopes)
		}
		var e Envelope
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("legacybridge: línea %d: %w", line, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("legacybridge: línea %d supera %d bytes", line+1, MaxEnvelopeBytes)
		}
		return nil, err
	}
	return out, nil
}

// VersionOrder es cómo se ordena una versión candidata contra la guardada.
type VersionOrder int

const (
	VersionStale VersionOrder = -1
	VersionSame  VersionOrder = 0
	VersionNewer VersionOrder = 1
)

// CompareVersions ordena la SourceVersion candidata contra la guardada de la
// MISMA fila. La parte antes de "#" se compara como instante si ambas son
// RFC3339 (si no, como texto). Con el mismo instante y distinta huella la
// candidata cuenta como más nueva: el cambio llegó dentro del mismo tick del
// reloj del origen y, como cada envelope trae el estado completo, la siguiente
// emisión corrige un eventual desorden. Por eso no es simétrica.
func CompareVersions(candidate, stored string) VersionOrder {
	if stored == "" {
		return VersionNewer
	}
	if candidate == stored {
		return VersionSame
	}
	ct, ch, _ := strings.Cut(candidate, "#")
	st, sh, _ := strings.Cut(stored, "#")
	if c := compareStamp(ct, st); c != 0 {
		return VersionOrder(c)
	}
	if ch == sh {
		return VersionSame
	}
	return VersionNewer
}

func compareStamp(a, b string) int {
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	if ea == nil && eb == nil {
		return ta.Compare(tb)
	}
	return strings.Compare(a, b)
}

// SystemOf devuelve el sistema de un external_id con prefijo ("erp:clients:8"
// → "erp"). Vacío si no trae prefijo.
func SystemOf(externalID string) string {
	sys, _, ok := strings.Cut(strings.TrimSpace(externalID), ":")
	if !ok {
		return ""
	}
	return sys
}
