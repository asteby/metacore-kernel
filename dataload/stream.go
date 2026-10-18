package dataload

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
)

// Fuentes en streaming. BuildPlan y Runner.Start con una Source normal leen el
// archivo entero a memoria (tope Profile.MaxRows). Una StreamSource se recorre
// fila por fila, tantas veces como haga falta, sin tenerla entera: el plan se
// calcula en dos pasadas (la primera detecta duplicados y valores repetidos; la
// segunda decide contra el destino por páginas) con la MISMA semántica que
// BuildPlan: mismas acciones por fila, mismos conteos, misma huella de entrada y
// de plan. Lo que sí se guarda en memoria es una entrada por identidad (y por
// valor de cada comprobación unique), no las filas.

// StreamSource es una Source que se recorre fila por fila. Each lee desde el
// principio en cada llamada y debe entregar siempre las mismas filas en el mismo
// orden (la corrida lo verifica con la huella de la entrada).
type StreamSource interface {
	Source
	Each(ctx context.Context, fn func(row map[string]any) error) error
}

// Opener abre la fuente desde el principio (un archivo, un objeto del almacén).
type Opener func() (io.ReadCloser, error)

// FileOpener abre un archivo local.
func FileOpener(path string) Opener {
	return func() (io.ReadCloser, error) { return os.Open(path) }
}

type streamSource struct {
	name string
	open Opener
	read func(r io.Reader, fn func(map[string]any) error) error
}

func (s streamSource) Name() string { return s.name }

func (s streamSource) Each(ctx context.Context, fn func(map[string]any) error) error {
	rc, err := s.open()
	if err != nil {
		return wrapErr(err, CodeSourceReopen, "source", s.name)
	}
	defer rc.Close()
	return s.read(rc, func(row map[string]any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(row)
	})
}

// Rows lee todo (compatibilidad con Source); para archivos grandes usa Each.
func (s streamSource) Rows(ctx context.Context) ([]map[string]any, error) {
	var out []map[string]any
	err := s.Each(ctx, func(r map[string]any) error { out = append(out, r); return nil })
	return out, err
}

// NewCSVStreamSource lee un CSV fila por fila con las mismas reglas que
// NewCSVSource (BOM, filas irregulares): las dos producen las mismas filas.
func NewCSVStreamSource(name string, open Opener) StreamSource {
	return streamSource{name: name, open: open, read: func(r io.Reader, fn func(map[string]any) error) error {
		cr := csv.NewReader(r)
		cr.FieldsPerRecord = -1
		var headers []string
		for {
			rec, err := cr.Read()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return wrapErr(err, CodeSourceRead, "source", name)
			}
			if headers == nil {
				headers = append([]string(nil), rec...)
				if len(headers) > 0 {
					headers[0] = importer.StripBOM(headers[0])
				}
				continue
			}
			row := make(map[string]any, len(headers))
			for i, key := range headers {
				if i < len(rec) {
					row[key] = rec[i]
				}
			}
			if err := fn(row); err != nil {
				return err
			}
		}
	}}
}

// DefaultMaxLineBytes es el tope de una línea de JSON líneas.
const DefaultMaxLineBytes = 8 << 20

// NewJSONLinesSource lee JSON líneas (un objeto por línea; las líneas vacías se
// saltan). Cada objeto se decodifica igual que en NewJSONSource.
func NewJSONLinesSource(name string, open Opener) StreamSource {
	return streamSource{name: name, open: open, read: func(r io.Reader, fn func(map[string]any) error) error {
		br := bufio.NewReaderSize(r, 64<<10)
		for line := 1; ; line++ {
			b, err := readLine(br, DefaultMaxLineBytes)
			if len(b) == 0 && errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				if errors.Is(err, errLineTooLong) {
					return newErr(CodeSourceLine, "source", name, "line", line, "detail", msg(CodeSourceLineTooLong, "limit", DefaultMaxLineBytes))
				}
				return wrapErr(err, CodeSourceRead, "source", name)
			}
			if b = bytes.TrimSpace(b); len(b) > 0 {
				var row map[string]any
				if uerr := json.Unmarshal(b, &row); uerr != nil || row == nil {
					return newErr(CodeSourceLine, "source", name, "line", line, "detail", msg(CodeSourceLineNotObject))
				}
				if ferr := fn(row); ferr != nil {
					return ferr
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
		}
	}}
}

var errLineTooLong error = internalErr("line too long")

// readLine lee hasta '\n' sin pasar de max bytes.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > max {
			return nil, errLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}

// asStream recorre cualquier Source; una que no es StreamSource se lee entera.
func asStream(src Source) StreamSource {
	if ss, ok := src.(StreamSource); ok {
		return ss
	}
	return eachRows{src}
}

type eachRows struct{ Source }

func (e eachRows) Each(ctx context.Context, fn func(map[string]any) error) error {
	rows, err := e.Rows(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// inputHasher calcula la misma huella que inputHash, fila por fila.
type inputHasher struct {
	h   hash.Hash
	enc *json.Encoder
	n   int
}

func newInputHasher() *inputHasher {
	h := sha256.New()
	return &inputHasher{h: h, enc: json.NewEncoder(h)}
}

func (ih *inputHasher) add(row map[string]any) { _ = ih.enc.Encode(row); ih.n++ }
func (ih *inputHasher) sum() string            { return hex.EncodeToString(ih.h.Sum(nil)) }

// scanInput recorre la fuente solo para su huella y su número de filas.
func scanInput(ctx context.Context, src StreamSource, limit int) (string, int, error) {
	ih := newInputHasher()
	err := src.Each(ctx, func(r map[string]any) error {
		if ih.n >= limit {
			return newErr(CodeSourceTooManyRows, "source", src.Name(), "limit", limit)
		}
		ih.add(r)
		return nil
	})
	if err != nil {
		return "", 0, readErr(src, err)
	}
	return ih.sum(), ih.n, nil
}

// readErr envuelve un error de lectura salvo que ya sea del motor o de contexto.
func readErr(src Source, err error) error {
	var e *Error
	var internal internalErr
	if errors.As(err, &e) || errors.As(err, &internal) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return wrapErr(err, CodeSourceRead, "source", src.Name())
}

// DefaultPageSize es el tamaño de página de StreamPlan.
const DefaultPageSize = 1000

// ScanSummary es lo que se sabe tras la primera pasada, antes de tocar el destino.
type ScanSummary struct {
	Rows       int    `json:"rows"`
	Invalid    int    `json:"invalid"`
	Duplicates int    `json:"duplicates"`
	InputHash  string `json:"inputHash"`
}

// StreamOptions controla StreamPlan.
type StreamOptions struct {
	// PageSize: filas por página (y por consulta al destino); 0 = DefaultPageSize.
	PageSize int
	// OnScan recibe el resumen de la primera pasada; si devuelve error se corta
	// sin consultar el destino.
	OnScan func(ctx context.Context, s ScanSummary) error
	// OnPage recibe cada página ya decidida, en orden de fila. Las filas no se
	// guardan en el Plan devuelto: quien necesite el diff completo lo guarda aquí.
	OnPage func(ctx context.Context, page []RowPlan) error
}

// StreamPlan es BuildPlan para archivos que no caben en memoria: misma
// semántica por fila y mismos Counts, InputHash y PlanHash, pero el Plan
// devuelto no trae Rows (llegan por OnPage). Acepta cualquier Source; una que no
// es StreamSource se lee entera. El tope de filas es Profile.MaxStreamRows.
func StreamPlan(ctx context.Context, req PlanRequest, opt StreamOptions) (*Plan, error) {
	sp, err := newStreamPlanner(req)
	if err != nil {
		return nil, err
	}
	if err := sp.scan(ctx, opt.PageSize); err != nil {
		return nil, err
	}
	if opt.OnScan != nil {
		if err := opt.OnScan(ctx, sp.summary); err != nil {
			return nil, err
		}
	}
	return sp.plan(ctx, nil, opt.OnPage)
}

// streamPlanner guarda el estado de la primera pasada para poder recorrer el
// archivo otra vez (el plan, o las pasadas de escritura por orden de aplicación).
type streamPlanner struct {
	*planner
	src     StreamSource
	first   map[string]int   // identidad -> primera fila que la trae
	unique  []map[string]int // por comprobación unique: valor -> filas que lo traen
	summary ScanSummary
	// block es el tamaño de página; blocks, la huella de cada bloque de block
	// filas de la primera pasada. Las pasadas siguientes comparan cada bloque
	// antes de entregar sus filas, así un archivo que cambió se detecta antes de
	// escribir nada de él (no solo al final).
	block  int
	blocks [][sha256.Size]byte
}

// blockHasher es la huella de un bloque de filas.
type blockHasher struct {
	h   hash.Hash
	enc *json.Encoder
}

func newBlockHasher() *blockHasher {
	h := sha256.New()
	return &blockHasher{h: h, enc: json.NewEncoder(h)}
}

func (b *blockHasher) add(row map[string]any) { _ = b.enc.Encode(row) }

func (b *blockHasher) take() [sha256.Size]byte {
	var out [sha256.Size]byte
	copy(out[:], b.h.Sum(nil))
	b.h.Reset()
	return out
}

func newStreamPlanner(req PlanRequest) (*streamPlanner, error) {
	if req.Org == "" || req.Source == nil {
		return nil, newErr(CodeRequestIncomplete)
	}
	prof, err := req.Profile.Resolve(req.Overrides)
	if err != nil {
		return nil, err
	}
	if req.Extract != nil {
		if err := req.Extract.CheckProfile(prof); err != nil {
			return nil, err
		}
	}
	return &streamPlanner{planner: newPlanner(req, prof), src: asStream(req.Source), first: map[string]int{}}, nil
}

// scan es la primera pasada: huella de la entrada, identidades (para los
// duplicados) y valores de las comprobaciones unique.
func (sp *streamPlanner) scan(ctx context.Context, pageSize int) error {
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	sp.block, sp.blocks = pageSize, nil
	bh := newBlockHasher()
	ih := newInputHasher()
	var checks []DatasetCheck
	if sp.req.Extract != nil {
		checks = sp.req.Extract.Doc.DatasetChecks
	}
	sp.unique = make([]map[string]int, len(checks))
	for i := range checks {
		sp.unique[i] = map[string]int{}
	}
	limit := sp.prof.maxStreamRows()
	err := sp.src.Each(ctx, func(raw map[string]any) error {
		if ih.n >= limit {
			return newErr(CodeSourceTooManyRows, "source", sp.src.Name(), "limit", limit)
		}
		i := ih.n
		ih.add(raw)
		bh.add(raw)
		if ih.n%sp.block == 0 {
			sp.blocks = append(sp.blocks, bh.take())
		}
		rp, _ := sp.planRow(i, raw)
		if rp.Action == ActionInvalid {
			sp.summary.Invalid++
			return nil
		}
		if _, dup := sp.first[rp.Key]; dup {
			sp.summary.Duplicates++
		} else {
			sp.first[rp.Key] = rp.Row
		}
		for c, chk := range checks {
			if v, ok := uniqueValue(rp.Record, chk.Field); ok {
				sp.unique[c][v]++
			}
		}
		return nil
	})
	if err != nil {
		return readErr(sp.src, err)
	}
	if ih.n%sp.block != 0 {
		sp.blocks = append(sp.blocks, bh.take())
	}
	sp.summary.Rows, sp.summary.InputHash = ih.n, ih.sum()
	return nil
}

// uniqueValue es el valor que compara una comprobación unique (igual que CheckDataset).
func uniqueValue(rec map[string]any, field string) (string, bool) {
	v, ok := getPath(rec, field)
	if !ok || v == nil {
		return "", false
	}
	s := valueText(v)
	return s, strings.TrimSpace(s) != ""
}

// datasetItems son los motivos de conjunto de una fila válida, en el orden de CheckDataset.
func (sp *streamPlanner) datasetItems(rec map[string]any) []ReviewItem {
	var out []ReviewItem
	for c, chk := range sp.req.Extract.Doc.DatasetChecks {
		if v, ok := uniqueValue(rec, chk.Field); ok && sp.unique[c][v] >= 2 {
			out = append(out, sp.req.Extract.uniqueItem(chk))
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Code < out[b].Code })
	return out
}

// plan es la segunda pasada: arma cada fila, marca duplicados y motivos de
// conjunto, decide contra el destino por páginas (del tamaño de scan) y entrega
// cada página después de comprobar que sus filas son las de la primera pasada.
// only, si no es nil, deja solo las filas que acepta (las demás ni se
// consultan); con filtro el Plan devuelto no lleva conteos ni huella.
func (sp *streamPlanner) plan(ctx context.Context, only func(*RowPlan) bool, onPage func(context.Context, []RowPlan) error) (*Plan, error) {
	pageSize := sp.block
	changed := newErr(CodeSourceChanged, "source", sp.src.Name())
	bh := newBlockHasher()
	verify := func(n int) error { // n filas leídas; cierra el bloque en curso
		b := (n - 1) / sp.block
		if b >= len(sp.blocks) || bh.take() != sp.blocks[b] {
			return changed
		}
		return nil
	}
	plan := sp.newPlan(sp.src.Name())
	plan.InputHash = sp.summary.InputHash
	ih := newInputHasher()
	h := sha256.New()
	var page []RowPlan
	flush := func() error {
		if len(page) == 0 {
			return nil
		}
		var keys []string
		for _, rp := range page {
			if rp.Action == "" {
				keys = append(keys, rp.Key)
			}
		}
		existing, err := sp.lookup(ctx, keys)
		if err != nil {
			return err
		}
		for i := range page {
			plan.count(&page[i], existing, h)
		}
		if onPage != nil {
			if err := onPage(ctx, page); err != nil {
				return err
			}
		}
		page = page[:0]
		return nil
	}
	err := sp.src.Each(ctx, func(raw map[string]any) error {
		i := ih.n
		if i >= sp.summary.Rows {
			return changed // más filas que en la primera pasada
		}
		ih.add(raw)
		bh.add(raw)
		rp, ignored := sp.planRow(i, raw)
		plan.IgnoredColumns += ignored
		if rp.Action == "" {
			if first := sp.first[rp.Key]; first != rp.Row {
				rp.markDuplicate(first)
			}
			if sp.req.Extract != nil {
				rp.Review = append(rp.Review, sp.datasetItems(rp.Record)...)
			}
		}
		if only == nil || only(&rp) {
			page = append(page, rp)
		}
		// Solo se entrega lo ya comprobado: al cerrar cada bloque.
		if ih.n%sp.block != 0 {
			return nil
		}
		if err := verify(ih.n); err != nil {
			return err
		}
		if len(page) >= pageSize {
			return flush()
		}
		return nil
	})
	if err == nil && ih.n != sp.summary.Rows {
		err = changed // menos filas que en la primera pasada
	}
	if err == nil && ih.n%sp.block != 0 {
		err = verify(ih.n)
	}
	if err == nil {
		err = flush()
	}
	if err != nil {
		return nil, readErr(sp.src, err)
	}
	if ih.sum() != sp.summary.InputHash {
		return nil, newErr(CodeSourceChanged, "source", sp.src.Name())
	}
	if only == nil {
		plan.PlanHash = hex.EncodeToString(h.Sum(nil))
	} else {
		plan.Counts, plan.MediaURLs, plan.IgnoredColumns = map[Action]int{}, 0, 0
	}
	return plan, nil
}
