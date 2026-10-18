package dataload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Action es lo que el plan hará con una fila.
type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionUnchanged Action = "unchanged"
	ActionInvalid   Action = "invalid"   // la fila no pasó validación; no se escribe
	ActionDuplicate Action = "duplicate" // misma identidad que una fila anterior del archivo; gana la primera
	// ActionReview: la fila es válida pero la etapa extract encontró un motivo que
	// bloquea la carga automática. No se escribe; queda en espera de una decisión
	// explícita (ReviewStore, Runner.Decide y Runner.ApplyApproved). Outcome dice
	// qué haría aplicarla.
	ActionReview Action = "review"
)

// Existing es lo que el destino ya tiene para una identidad.
type Existing struct {
	// Record son los campos actuales, con las mismas rutas que el perfil.
	Record map[string]any
	// Hash es la huella que el destino guardó la última vez que aplicó una
	// Change (opcional). Si coincide con la nueva, la fila es unchanged sin
	// comparar campo por campo.
	Hash string
}

// Change es una escritura que el destino debe aplicar de forma idempotente
// (upsert por Key).
type Change struct {
	Key    string
	Action Action // create o update
	Record map[string]any
	Hash   string
}

// Target es la persistencia del destino; la implementan ops/addons.
type Target interface {
	// Lookup devuelve lo existente para las identidades pedidas (las ausentes no
	// aparecen en el mapa). No debe escribir.
	Lookup(ctx context.Context, org, entity string, keys []string) (map[string]Existing, error)
	// Apply hace upsert de un lote. Debe ser idempotente por Key y atómico por
	// lote; un error deja el lote sin aplicar.
	Apply(ctx context.Context, org, entity string, changes []Change) error
}

// ApplyMark es la constancia de un lote escrito por una corrida: cuántas filas
// creó y actualizó. La escribe el destino en la misma transacción que el lote,
// así que sobrevive aunque el proceso muera antes de guardar el checkpoint.
type ApplyMark struct {
	RunID string `json:"runId"`
	// Batch es el número de lote de la corrida (acumulado entre reanudaciones:
	// Checkpoint.Batches + 1).
	Batch int `json:"batch"`
	// Generation es Checkpoint.Resumes del proceso que escribió el lote: entre
	// dos marcas del mismo lote vale la de generación mayor.
	Generation int    `json:"generation,omitempty"`
	Created    int    `json:"created"`
	Updated    int    `json:"updated"`
	LastKey    string `json:"lastKey,omitempty"`
}

// MarkingTarget es un Target que además guarda marcas de aplicación. Es
// opcional: sin él, si el proceso muere entre escribir un lote y guardar su
// checkpoint, la reanudación ve esas filas como unchanged y el conteo de
// Created/Updated de la corrida queda corto (los datos sí quedan bien). Con él,
// la reanudación lee las marcas posteriores al checkpoint y el conteo es exacto.
type MarkingTarget interface {
	Target
	// ApplyMarked hace lo mismo que Apply y guarda mark en la MISMA transacción
	// (lote y marca, o nada). Es idempotente por (org, RunID, Generation, Batch):
	// repetirla reemplaza la marca.
	ApplyMarked(ctx context.Context, org, entity string, mark ApplyMark, changes []Change) error
	// ApplyMarks devuelve las marcas de la corrida con Batch > after (de todas las
	// generaciones), en cualquier orden.
	ApplyMarks(ctx context.Context, org, runID string, after int) ([]ApplyMark, error)
}

// FieldDiff es un campo que cambia en una actualización.
type FieldDiff struct {
	Field string `json:"field"`
	Old   any    `json:"old"`
	New   any    `json:"new"`
}

// RowPlan es el plan de una fila de la fuente.
type RowPlan struct {
	Row    int                 `json:"row"` // 1-based sobre las filas de datos
	Key    string              `json:"key,omitempty"`
	Action Action              `json:"action"`
	Hash   string              `json:"hash,omitempty"`
	Record map[string]any      `json:"record,omitempty"`
	Media  map[string][]string `json:"media,omitempty"` // campo -> URLs por respaldar
	Diffs  []FieldDiff         `json:"diffs,omitempty"`
	Issues []importer.RowIssue `json:"issues,omitempty"`
	// Review son los motivos de la etapa extract (revisar e info), con su motivo.
	Review []ReviewItem `json:"review,omitempty"`
	// Outcome, solo en filas review: lo que pasaría al aplicarla (create, update o unchanged).
	Outcome Action `json:"outcome,omitempty"`
}

// Blocking dice si algún motivo de la fila bloquea la carga automática.
func (r RowPlan) Blocking() bool {
	for _, it := range r.Review {
		if it.Severity == SeverityRevisar {
			return true
		}
	}
	return false
}

// Plan es el resultado del dry-run: qué pasaría, sin escribir nada.
type Plan struct {
	Org            string `json:"org"`
	Entity         string `json:"entity"`
	Profile        string `json:"profile"`
	ProfileVersion string `json:"profileVersion"`
	ProfileHash    string `json:"profileHash"`
	// ExtractHash es la huella del documento de extracción (vacía si no hay).
	ExtractHash    string         `json:"extractHash,omitempty"`
	Source         string         `json:"source"`
	InputHash      string         `json:"inputHash"`
	PlanHash       string         `json:"planHash"`
	Rows           []RowPlan      `json:"rows"`
	Counts         map[Action]int `json:"counts"`
	IgnoredColumns int            `json:"ignoredColumns"`
	MediaURLs      int            `json:"mediaUrls"`

	applyOrder []OrderRule
}

// PlanRequest reúne lo necesario para planificar.
type PlanRequest struct {
	Org       string
	Profile   Profile
	Overrides *Overrides
	Source    Source
	Target    Target
	// Extract es la etapa de extracción opcional (ParseExtract). Debe
	// corresponder a la versión del perfil.
	Extract *Extractor
	// LookupChunk: cuántas identidades se consultan al destino por llamada; 0 = 500.
	// Evita una consulta de decenas de miles de llaves en lotes grandes.
	LookupChunk int
}

// DefaultLookupChunk es el tamaño de las consultas de identidades al destino.
const DefaultLookupChunk = 500

// BuildPlan lee la fuente, aplica el perfil (con los overrides de la org),
// valida y compara contra el destino. Nunca escribe y nunca descarga medios.
// Tiene todo el archivo en memoria (tope Profile.MaxRows); para archivos más
// grandes está StreamPlan, con la misma semántica.
func BuildPlan(ctx context.Context, req PlanRequest) (*Plan, error) {
	if req.Org == "" {
		return nil, newErr(CodeRequestIncomplete)
	}
	prof, err := req.Profile.Resolve(req.Overrides)
	if err != nil {
		return nil, err
	}
	rows, err := req.Source.Rows(ctx)
	if err != nil {
		return nil, wrapErr(err, CodeSourceRead, "source", req.Source.Name())
	}
	if len(rows) > prof.maxRows() {
		return nil, importer.ErrTooManyRows{Got: len(rows), Limit: prof.maxRows()}
	}
	if req.Extract != nil {
		if err := req.Extract.CheckProfile(prof); err != nil {
			return nil, err
		}
	}
	pl := newPlanner(req, prof)
	plan := pl.newPlan(req.Source.Name())
	plan.InputHash = inputHash(rows)

	seen := map[string]int{}
	var keys []string
	for i, raw := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rp, ignored := pl.planRow(i, raw)
		plan.IgnoredColumns += ignored
		if rp.Action == "" {
			if first, dup := seen[rp.Key]; dup {
				rp.markDuplicate(first)
			} else {
				seen[rp.Key] = rp.Row
				keys = append(keys, rp.Key)
			}
		}
		plan.add(rp)
	}

	if req.Extract != nil {
		facts := make([]RowFacts, len(plan.Rows))
		for i, rp := range plan.Rows {
			if rp.Action != ActionInvalid {
				facts[i] = RowFacts{Record: rp.Record}
			}
		}
		for _, di := range req.Extract.CheckDataset(facts) {
			plan.Rows[di.Index].Review = append(plan.Rows[di.Index].Review, di.Item)
		}
	}

	existing, err := pl.lookup(ctx, keys)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for i := range plan.Rows {
		plan.count(&plan.Rows[i], existing, h)
	}
	plan.PlanHash = hex.EncodeToString(h.Sum(nil))
	return plan, nil
}

// planner reúne lo que comparten BuildPlan y StreamPlan para planificar una
// fila: la misma función produce el mismo resultado en los dos caminos.
type planner struct {
	req  PlanRequest
	prof Profile
	spec modelbase.ImportSpec
}

func newPlanner(req PlanRequest, prof Profile) *planner {
	return &planner{req: req, prof: prof, spec: prof.importSpec()}
}

func (pl *planner) newPlan(source string) *Plan {
	p := &Plan{
		Org: pl.req.Org, Entity: pl.prof.Entity, Profile: pl.prof.Name, ProfileVersion: pl.prof.Version,
		ProfileHash: pl.prof.Hash(), Source: source, Counts: map[Action]int{}, applyOrder: pl.prof.ApplyOrder,
	}
	if pl.req.Extract != nil {
		p.ExtractHash = pl.req.Extract.Hash()
	}
	return p
}

// planRow arma la fila i (0-based): registro, medios, motivos de la etapa
// extract e identidad. Action queda en invalid o vacía (por decidir contra el
// destino); la detección de duplicados y las comprobaciones de conjunto las
// hace quien recorre el archivo.
func (pl *planner) planRow(i int, raw map[string]any) (RowPlan, int) {
	rp := RowPlan{Row: i + 1}
	rec, media, ignored, issues := pl.prof.buildRecord(pl.spec, raw, nil, i)
	if len(issues) > 0 {
		for j := range issues {
			issues[j].Row = rp.Row
		}
		rp.Action, rp.Issues = ActionInvalid, issues
		return rp, ignored
	}
	if pl.req.Extract != nil {
		rp.Review = pl.req.Extract.Apply(pl.prof, raw, rec).Review
	}
	key, ok := identity(pl.prof.Key, rec)
	if !ok {
		rp.Action = ActionInvalid
		rp.Issues = []importer.RowIssue{rowIssue(rp.Row, "", CodeRowMissingIdentity, "keys", strings.Join(pl.prof.Key, ", "))}
		return rp, ignored
	}
	rp.Key, rp.Record, rp.Media = key, rec, media
	rp.Hash = recordHash(rec, media)
	return rp, ignored
}

// markDuplicate marca la fila como repetida de la fila first.
func (rp *RowPlan) markDuplicate(first int) {
	rp.Action = ActionDuplicate
	rp.Issues = []importer.RowIssue{rowIssue(rp.Row, "", CodeRowDuplicate, "first", first)}
}

// lookup consulta al destino por tandas de LookupChunk identidades.
func (pl *planner) lookup(ctx context.Context, keys []string) (map[string]Existing, error) {
	existing := map[string]Existing{}
	chunk := pl.req.LookupChunk
	if chunk <= 0 {
		chunk = DefaultLookupChunk
	}
	for lo := 0; lo < len(keys); lo += chunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		got, err := pl.req.Target.Lookup(ctx, pl.req.Org, pl.prof.Entity, keys[lo:min(lo+chunk, len(keys))])
		if err != nil {
			return nil, wrapErr(err, CodeTargetLookup)
		}
		for k, v := range got {
			existing[k] = v
		}
	}
	return existing, nil
}

// count decide la acción de una fila pendiente contra lo existente, la suma a
// la huella del plan y a los conteos.
func (p *Plan) count(rp *RowPlan, existing map[string]Existing, h io.Writer) {
	if rp.Action == "" {
		ex, found := existing[rp.Key]
		switch {
		case !found:
			rp.Action = ActionCreate
		case ex.Hash != "" && ex.Hash == rp.Hash:
			rp.Action = ActionUnchanged
		default:
			rp.Diffs = diffRecords(ex.Record, rp.Record)
			if ex.Hash == "" && len(rp.Diffs) == 0 {
				rp.Action = ActionUnchanged
			} else {
				rp.Action = ActionUpdate
			}
		}
		// Una fila con motivo bloqueante no se escribe sola. Si el destino ya la
		// tiene idéntica (p. ej. se aprobó antes) no hay nada que decidir.
		if rp.Blocking() && rp.Action != ActionUnchanged {
			rp.Outcome, rp.Action = rp.Action, ActionReview
		}
		fmt.Fprintf(h, "%s|%s|%s\n", rp.Key, rp.Action, rp.Hash)
	}
	p.Counts[rp.Action]++
	for _, u := range rp.Media {
		p.MediaURLs += len(u)
	}
}

func (p *Plan) add(rp RowPlan) { p.Rows = append(p.Rows, rp) }

// Reviews devuelve las filas que esperan una decisión, en orden de fila.
func (p *Plan) Reviews() []RowPlan {
	var out []RowPlan
	for _, r := range p.Rows {
		if r.Action == ActionReview {
			out = append(out, r)
		}
	}
	return out
}

// Changes devuelve las escrituras del plan (create y update), en orden de fila
// y, si el perfil declara ApplyOrder, ordenadas por esas reglas (estable).
func (p *Plan) Changes() []Change {
	var out []Change
	for _, r := range p.Rows {
		if r.Action == ActionCreate || r.Action == ActionUpdate {
			out = append(out, Change{Key: r.Key, Action: r.Action, Record: r.Record, Hash: r.Hash})
		}
	}
	if len(p.applyOrder) > 0 {
		sort.SliceStable(out, func(a, b int) bool { return lessRank(p.rank(out[a]), p.rank(out[b])) })
	}
	return out
}

// rank es la posición de un cambio según ApplyOrder (un valor fuera de Rank va al final).
func (p *Plan) rank(c Change) []int {
	rs := make([]int, len(p.applyOrder))
	for i, o := range p.applyOrder {
		rs[i] = len(o.Rank)
		v, _ := getPath(c.Record, o.Field)
		s := fmt.Sprint(v)
		for j, want := range o.Rank {
			if s == want {
				rs[i] = j
				break
			}
		}
	}
	return rs
}

// identity arma la identidad a partir de los campos clave; vacía = inválida.
func identity(keys []string, rec map[string]any) (string, bool) {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := getPath(rec, k)
		s := strings.TrimSpace(fmt.Sprint(v))
		if !ok || v == nil || s == "" {
			return "", false
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\x1f"), true
}

func getPath(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// recordHash es la huella canónica de lo que la fila quiere dejar en el destino
// (registro + URLs de medios).
func recordHash(rec map[string]any, media map[string][]string) string {
	b, _ := json.Marshal(struct {
		R map[string]any      `json:"r"`
		M map[string][]string `json:"m,omitempty"`
	}{rec, media})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// diffRecords compara solo los campos que el perfil escribe.
func diffRecords(old, next map[string]any) []FieldDiff {
	a, b := flatten("", old), flatten("", next)
	var out []FieldDiff
	for k, nv := range b {
		if ov, ok := a[k]; !ok || !equalJSON(ov, nv) {
			out = append(out, FieldDiff{Field: k, Old: a[k], New: nv})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func flatten(prefix string, m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			for sk, sv := range flatten(path, sub) {
				out[sk] = sv
			}
			continue
		}
		out[path] = v
	}
	return out
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) == string(y) {
		return true
	}
	// 10 (int) vs 10.0 (float64) y similares: compara por texto.
	return fmt.Sprint(a) == fmt.Sprint(b)
}
